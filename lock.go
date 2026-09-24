// Copyright 2026 Dolthub, Inc. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the root of this repository.

// Package fslock takes exclusive advisory locks on files, so that cooperating
// processes can take turns. It keeps the interface of
// github.com/dolthub/fslock, which it replaces.
package fslock

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

// Kind is what went wrong.
type Kind int

const (
	Locked   Kind = iota + 1 // someone else holds the lock
	TimedOut                 // the wait ran out of time
	Canceled                 // the caller's context ended the wait
	Misuse                   // the Lock was closed, unlocked when it held nothing, or locked twice
	Failed                   // a system call failed
)

var kindText = map[Kind]string{
	Locked:   "file is locked",
	TimedOut: "timed out waiting for the lock",
	Canceled: "wait canceled",
}

// An Error is every error this package reports.
type Error struct {
	Op   string
	Path string
	Kind Kind
	Err  error
}

// ErrLocked and ErrTimeout are returned as these exact values, so == works as
// well as errors.Is. An *Error of the same Kind matches them with errors.Is.
var (
	ErrLocked  = &Error{Kind: Locked}
	ErrTimeout = &Error{Kind: TimedOut}
)

func (e *Error) Error() string {
	msg := kindText[e.Kind]
	switch {
	case msg == "":
		msg = e.Err.Error()
	case e.Err != nil:
		msg += ": " + e.Err.Error()
	}
	if e.Op == "" {
		return "fslock: " + msg
	}
	return "fslock " + e.Op + " " + e.Path + ": " + msg
}

func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinel values by Kind.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind && t.Op == "" && t.Err == nil
}

// IsTimeout reports whether the lock was not taken because time ran out.
func (e *Error) IsTimeout() bool { return e.Kind == TimedOut }

// IsTemporary reports whether trying again later might work.
func (e *Error) IsTemporary() bool { return e.Kind == Locked || e.Kind == TimedOut }

// IsTimeout reports whether err is an [*Error] that timed out.
func IsTimeout(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.IsTimeout()
}

// IsTemporary reports whether err is an [*Error] worth retrying.
func IsTemporary(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.IsTemporary()
}

// A Lock is a handle on one lock file. It may be locked and unlocked any
// number of times, and is safe for concurrent use. Two Locks on the same file
// exclude each other whether or not they are in the same process.
type Lock struct {
	path string
	mu   sync.Mutex
	f    *os.File
	held bool
}

// New opens the lock file at path, creating it if it is not there, and
// returns an unlocked Lock on it. Close it when you are done.
func New(path string) (*Lock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	return &Lock{path: path, f: f}, nil
}

// Lock takes the lock, waiting as long as it takes.
func (l *Lock) Lock() error {
	return l.acquire(context.Background(), time.Time{}, false, ErrTimeout)
}

// TryLock takes the lock if it is free, and reports [ErrLocked] if it is not.
func (l *Lock) TryLock() error {
	return l.acquire(context.Background(), time.Time{}, true, ErrLocked)
}

// LockWithTimeout takes the lock, waiting up to d, and reports [ErrTimeout]
// if that is not long enough. A d that is not positive means one attempt.
func (l *Lock) LockWithTimeout(d time.Duration) error {
	if d <= 0 {
		return l.acquire(context.Background(), time.Time{}, true, ErrTimeout)
	}
	return l.acquire(context.Background(), time.Now().Add(d), false, ErrTimeout)
}

// LockWithContext takes the lock, waiting until ctx is done.
func (l *Lock) LockWithContext(ctx context.Context) error {
	return l.acquire(ctx, time.Time{}, false, ErrTimeout)
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.held {
		return l.misuse("unlock", "lock is not held")
	}
	l.held = false
	if err := sysUnlock(l.f); err != nil {
		return l.fail("unlock", err)
	}
	return nil
}

// Close releases the lock if it is held and closes the lock file, which is
// left in place. Closing twice is harmless.
func (l *Lock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		return nil
	}
	var err error
	if l.held {
		l.held = false
		err = sysUnlock(l.f)
	}
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	if err != nil {
		return l.fail("close", err)
	}
	return nil
}

func (l *Lock) acquire(ctx context.Context, deadline time.Time, once bool, busy *Error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		return l.misuse("lock", "lock is closed")
	}
	if l.held {
		return l.misuse("lock", "lock is already held")
	}

	retry := time.Millisecond
	for reopens := 0; ; {
		if err := ctx.Err(); err != nil {
			return l.contextError(err)
		}

		// Wait in the kernel when nothing could call the wait off, so that
		// waiters queue there instead of racing each other. A bounded or
		// cancellable wait cannot: neither platform can interrupt one.
		ok, err := sysLock(l.f, !once && deadline.IsZero() && ctx.Done() == nil)
		if err != nil {
			return l.fail("lock", err)
		}

		if ok {
			// A lock on a file that has been unlinked excludes nobody,
			// so the file we locked has to be the one at the path.
			same, err := l.atPath()
			if err != nil {
				sysUnlock(l.f)
				return l.fail("lock", err)
			}
			if same {
				l.held = true
				return nil
			}
			if err := sysUnlock(l.f); err != nil {
				return l.fail("lock", err)
			}
			if reopens++; reopens > 10 {
				return l.fail("lock", errors.New("lock file kept being replaced"))
			}
			if err := l.reopen(); err != nil {
				return err
			}
			continue
		}

		if once {
			return busy
		}

		pause := retry
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return busy
			}
			if pause > remaining {
				pause = remaining
			}
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return l.contextError(ctx.Err())
		case <-timer.C:
		}
		if retry < 16*time.Millisecond {
			retry *= 2
		}
	}
}

// atPath reports whether the open file is still the file the path names.
func (l *Lock) atPath() (bool, error) {
	open, err := l.f.Stat()
	if err != nil {
		return false, err
	}
	named, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(open, named), nil
}

func (l *Lock) reopen() error {
	l.f.Close()
	f, err := openLockFile(l.path)
	l.f = f
	return err
}

func openLockFile(path string) (*os.File, error) {
	f, err := sysOpen(path)
	if err != nil {
		return nil, &Error{Op: "new", Path: path, Kind: Failed, Err: err}
	}
	return f, nil
}

// control runs fn on the file's descriptor or handle, which SyscallConn keeps
// open for the call.
func control(f *os.File, fn func(fd uintptr)) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	return rc.Control(fn)
}

func (l *Lock) contextError(err error) error {
	kind := Canceled
	if errors.Is(err, context.DeadlineExceeded) {
		kind = TimedOut
	}
	return &Error{Op: "lock", Path: l.path, Kind: kind, Err: err}
}

func (l *Lock) fail(op string, err error) error {
	return &Error{Op: op, Path: l.path, Kind: Failed, Err: err}
}

func (l *Lock) misuse(op, msg string) error {
	return &Error{Op: op, Path: l.path, Kind: Misuse, Err: errors.New(msg)}
}
