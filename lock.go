// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fslock

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"sync"
	"time"
)

const (
	// lockFileMode is the mode a lock file is created with, before umask.
	// Everyone who can write the directory can take part.
	lockFileMode os.FileMode = 0666

	// A wait that cannot block in the kernel retries on a schedule that
	// starts here and doubles up to the ceiling. The floor keeps a short
	// wait responsive; the ceiling keeps a long one from spinning.
	minRetry = time.Millisecond
	maxRetry = 16 * time.Millisecond

	// maxReopens bounds how many times one call will reopen a lock file
	// that was replaced underneath it. See the open/lock race below.
	maxReopens = 10
)

// A Lock is a handle on one lock file.
//
// Take the lock with [Lock.Lock], [Lock.TryLock], [Lock.LockWithTimeout] or
// [Lock.LockWithContext], give it back with [Lock.Unlock], and call
// [Lock.Close] when you are done with the file. A Lock may be taken and given
// back any number of times.
//
// A Lock is safe for concurrent use. Two Locks on the same file exclude each
// other whether or not they are in the same process, and one Lock cannot take
// a lock it already holds.
type Lock struct {
	path string

	mu     sync.Mutex
	f      *os.File
	id     fileID
	held   bool
	closed bool
}

// New opens the lock file at path, creating it if it is not there, and
// returns a Lock on it. The directory must already exist. No lock is taken.
//
// The returned Lock owns an open file, so close it when you are done.
func New(path string) (*Lock, error) {
	f, id, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	return &Lock{path: path, f: f, id: id}, nil
}

// Lock takes the lock, waiting as long as it takes.
func (l *Lock) Lock() error {
	return l.acquire(attempt{op: "lock", ctx: context.Background(), busy: ErrTimeout})
}

// TryLock takes the lock if it is free this instant, and reports [ErrLocked]
// if someone else holds it.
func (l *Lock) TryLock() error {
	return l.acquire(attempt{op: "trylock", ctx: context.Background(), once: true, busy: ErrLocked})
}

// LockWithTimeout takes the lock, waiting up to d, and reports [ErrTimeout]
// if that is not long enough. A d that is not positive means a single
// attempt: if the lock is not free this instant, the time is already up.
func (l *Lock) LockWithTimeout(d time.Duration) error {
	a := attempt{op: "lock", ctx: context.Background(), busy: ErrTimeout}
	if d <= 0 {
		a.once = true
	} else {
		a.deadline = time.Now().Add(d)
	}
	return l.acquire(a)
}

// LockWithContext takes the lock, waiting until ctx is done. A cancelled
// context reports an [*Error] of Kind [Canceled] wrapping ctx.Err(); a
// context whose deadline passes reports one of Kind [TimedOut], which
// errors.Is matches against [ErrTimeout].
//
// This is the one method fslock did not have. The others are implemented on
// top of it, so everything here is interruptible on the inside.
func (l *Lock) LockWithContext(ctx context.Context) error {
	return l.acquire(attempt{op: "lock", ctx: ctx, busy: ErrTimeout})
}

// Unlock gives the lock back. It reports an [*Error] of Kind [NotHeld] if
// this Lock does not hold the lock.
func (l *Lock) Unlock() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return stateError("unlock", l.path, Closed)
	}
	if !l.held {
		return stateError("unlock", l.path, NotHeld)
	}
	return l.release("unlock")
}

// Close gives the lock back if this Lock holds it and closes the underlying
// file. The lock file itself is left in place. Closing twice is harmless, and
// a closed Lock reports an [*Error] of Kind [Closed] from every other method.
func (l *Lock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return nil
	}
	l.closed = true

	var first error
	if l.held {
		if err := l.release("close"); err != nil {
			first = err
		}
	}
	if l.f != nil {
		if err := l.f.Close(); err != nil && first == nil {
			first = failure("close", l.path, err)
		}
		l.f = nil
	}
	return first
}

// An attempt is one call's terms for taking the lock.
type attempt struct {
	op       string
	ctx      context.Context
	deadline time.Time // zero means no deadline of our own
	once     bool      // one try, no waiting
	busy     *Error    // what to report when we give up: ErrLocked or ErrTimeout
}

// acquire takes the lock on the terms in a.
//
// # The open/lock race
//
// Opening a lock file and locking it are two steps, and the path can be
// replaced in between: another participant unlinks it and creates a new one,
// and now two holders hold locks on two different files, each believing it
// has the only one. Nothing in flock(2) or LockFileEx closes that window,
// because the lock belongs to the file we opened, not to the name we opened
// it by.
//
// So after taking the lock, and before reporting success, we check that the
// file we locked is still the file at the path. If it is not, we let go,
// reopen the path and try again - the lock we were holding was on a file
// nobody can reach any more, which means it excluded nobody.
func (l *Lock) acquire(a attempt) error {
	retry := minRetry
	reopens := 0

	for {
		if err := a.ctx.Err(); err != nil {
			return l.contextError(a, err)
		}

		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return stateError(a.op, l.path, Closed)
		}
		if l.held {
			l.mu.Unlock()
			return stateError(a.op, l.path, AlreadyHeld)
		}

		// The in-process slot comes first, so that a filesystem whose
		// locks are owned by the process rather than by the open file
		// cannot let a second Lock here think it won. See inproc.go.
		if !claimSlot(l.id) {
			l.mu.Unlock()
			if a.once {
				return a.busy
			}
			if err := l.pause(a, &retry); err != nil {
				return err
			}
			continue
		}

		// Wait in the kernel where the platform can be interrupted out
		// of it, and poll where it cannot. Either way a single attempt
		// never waits.
		block := sysWaitsInKernel && !a.once
		ok, err := sysAcquire(l.f, block, a.ctx, a.deadline)
		if err != nil {
			freeSlot(l.id)
			l.mu.Unlock()
			return failure(a.op, l.path, err)
		}
		if !ok {
			freeSlot(l.id)
			l.mu.Unlock()
			if a.once {
				return a.busy
			}
			if block {
				// The kernel wait ran out the clock for us.
				if cerr := a.ctx.Err(); cerr != nil {
					return l.contextError(a, cerr)
				}
				return a.busy
			}
			if err := l.pause(a, &retry); err != nil {
				return err
			}
			continue
		}

		// We hold a lock. On the file at the path, or on a ghost?
		current, err := l.stillAtPath()
		if err == nil && current {
			l.held = true
			l.mu.Unlock()
			return nil
		}

		_ = sysRelease(l.f)
		freeSlot(l.id)
		if err != nil {
			l.mu.Unlock()
			return failure(a.op, l.path, err)
		}
		if reopens++; reopens > maxReopens {
			l.mu.Unlock()
			return stateError(a.op, l.path, Replaced)
		}
		rerr := l.reopen(a.op)
		l.mu.Unlock()
		if rerr != nil {
			return rerr
		}
		// Straight back around: this was our own staleness, not
		// contention, so there is nothing to wait for.
	}
}

// pause waits before the next attempt, and reports the reason to stop if
// there is one. The caller must not hold l.mu.
func (l *Lock) pause(a attempt, retry *time.Duration) error {
	d := jitter(*retry)
	if !a.deadline.IsZero() {
		remaining := time.Until(a.deadline)
		if remaining <= 0 {
			return a.busy
		}
		if d > remaining {
			d = remaining
		}
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-a.ctx.Done():
		return l.contextError(a, a.ctx.Err())
	case <-timer.C:
	}

	if *retry < maxRetry {
		if *retry *= 2; *retry > maxRetry {
			*retry = maxRetry
		}
	}
	return nil
}

// release drops the lock. The caller holds l.mu and has checked the state.
func (l *Lock) release(op string) error {
	err := sysRelease(l.f)
	// The lock is gone either way: a failed release leaves a file that is
	// no longer trustworthy, and closing it drops the lock regardless.
	l.held = false
	freeSlot(l.id)
	if err != nil {
		return failure(op, l.path, err)
	}
	return nil
}

// stillAtPath reports whether the open file is the file the path names.
func (l *Lock) stillAtPath() (bool, error) {
	open, err := l.f.Stat()
	if err != nil {
		return false, err
	}
	named, err := os.Stat(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The lock file was removed. What we hold is a lock on a
			// file with no name, which excludes nobody.
			return false, nil
		}
		return false, err
	}
	return os.SameFile(open, named), nil
}

// reopen replaces the open file with the one the path names now. The caller
// holds l.mu. A Lock that cannot be reopened is finished: it is marked closed
// so that it reports that rather than reaching for a file it does not have.
func (l *Lock) reopen(op string) error {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	f, id, err := openLockFile(l.path)
	if err != nil {
		l.closed = true
		var e *Error
		if errors.As(err, &e) {
			e.Op = op
		}
		return err
	}
	l.f, l.id = f, id
	return nil
}

func (l *Lock) contextError(a attempt, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return timedOut(a.op, l.path, err)
	}
	return canceled(a.op, l.path, err)
}

// openLockFile opens the lock file, creating it if it is not there.
//
// This goes through os.OpenFile, and every system call on the result goes
// through the file's SyscallConn, rather than calling open(2) directly and
// keeping a bare descriptor. The reason is the race the os package documents
// where it opens files itself - "There's a race here with fork/exec, which we
// are content to live with" (os/file_unix.go, and the same comment in
// os/root_unix.go where (*os.Root).OpenFile gets its descriptor): a
// descriptor opened without close-on-exec set atomically can be inherited by
// a child that another goroutine is forking at that moment.
//
// The os package is content to live with it because an inherited descriptor
// is usually just a leak. For a lock it is worse than that. A flock(2) lock
// belongs to the open file description, and a child that inherits the
// descriptor shares it, so the lock stays held after this process unlocks and
// closes - for as long as the child lives. A program that shells out while
// holding a lock would leave locks behind it that nothing can clear.
//
// os.OpenFile passes O_CLOEXEC atomically where the platform has it and takes
// the fork lock where it does not, which is exactly the handling we want and
// exactly what is easy to get wrong by calling the syscall directly. Going
// through SyscallConn for the later calls keeps the descriptor from being
// closed and reused underneath a system call, too.
func openLockFile(path string) (*os.File, fileID, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, lockFileMode)
	if err != nil {
		return nil, fileID{}, failure("new", path, err)
	}
	id, err := sysIdentify(f)
	if err != nil {
		f.Close()
		return nil, fileID{}, failure("new", path, err)
	}
	return f, id, nil
}

// jitter spreads retries out, so that a crowd of waiters released at once
// does not line up on the same schedule and collide on every attempt.
func jitter(d time.Duration) time.Duration {
	spread := int64(d / 4)
	if spread <= 0 {
		return d
	}
	return d - time.Duration(rand.Int64N(spread))
}
