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

package filelock

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// defaultFileMode is the mode a lock file is created with, before umask.
	defaultFileMode os.FileMode = 0666

	// A wait retries on a schedule that starts here and doubles up to the
	// ceiling. The floor keeps a short wait - the common case, where the
	// holder is committing a manifest and will be gone in microseconds -
	// responsive; the ceiling keeps a long one from spinning.
	defaultMinRetry = time.Millisecond
	defaultMaxRetry = 16 * time.Millisecond
)

// An Option adjusts how a [Guard] behaves. Pass options to [Open].
type Option func(*config)

type config struct {
	mode     os.FileMode
	minRetry time.Duration
	maxRetry time.Duration
	stamp    bool
}

// WithFileMode sets the permissions the lock file is created with, before
// umask. The default is 0666, which lets every user who can write the
// directory participate. It has no effect on windows, or if the file already
// exists.
func WithFileMode(mode os.FileMode) Option {
	return func(c *config) { c.mode = mode }
}

// WithRetrySchedule sets the bounds of the backoff a wait retries on. Values
// that are not positive, or are out of order, are ignored.
func WithRetrySchedule(min, max time.Duration) Option {
	return func(c *config) {
		if min > 0 && max >= min {
			c.minRetry, c.maxRetry = min, max
		}
	}
}

// WithHolderStamp records who holds the claim in the lock file itself, so that
// a Guard that cannot acquire can say something better than "someone else has
// it" - see [Guard.ReadHolder].
//
// The stamp is diagnostic. It is written after the claim is taken and is not
// consulted when taking one, a failure to write it does not fail the acquire,
// and a stamp read back may describe a holder that has since gone away. Never
// make a correctness decision from it.
func WithHolderStamp() Option {
	return func(c *config) { c.stamp = true }
}

// A Guard is one participant's claim on one lock file.
//
// Open a Guard with [Open], take the claim with [Guard.AcquireNow],
// [Guard.Acquire] or [Guard.AcquireWithin], give it back with
// [Guard.Release], and close the Guard when you are done with the file
// entirely. A Guard may be acquired and released any number of times.
//
// A Guard is safe for concurrent use. Two Guards on the same file exclude each
// other whether or not they are in the same process; one Guard cannot take a
// claim it already holds.
type Guard struct {
	path string
	cfg  config
	id   fileID

	mu      sync.Mutex
	h       sysHandle
	holding bool
	closed  bool
}

// Open prepares a Guard for the lock file at path, creating the file if it is
// not there. The directory must already exist. No claim is taken; use one of
// the Acquire methods for that.
//
// The returned Guard owns an open file handle, so close it when you are done.
func Open(path string, opts ...Option) (*Guard, error) {
	cfg := config{
		mode:     defaultFileMode,
		minRetry: defaultMinRetry,
		maxRetry: defaultMaxRetry,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	h, err := sysOpen(path, cfg.mode)
	if err != nil {
		return nil, err
	}

	// The filesystem's own identity is what makes two paths to one file - a
	// symlink, a bind mount, a hard link - resolve to one claim. An absolute
	// path is the fallback when it is unavailable.
	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		abs = path
	}
	id, ierr := sysIdentify(h)
	if ierr != nil {
		id = fileID{path: filepath.Clean(abs)}
	}

	return &Guard{path: path, cfg: cfg, id: id, h: h}, nil
}

// Path returns the path the Guard was opened with.
func (g *Guard) Path() string { return g.path }

// Holding reports whether this Guard currently holds the claim.
func (g *Guard) Holding() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holding
}

// AcquireNow takes the claim if it is free this instant, reporting whether it
// got it. A false return with a nil error is the ordinary "someone else has
// it" answer, not a failure.
func (g *Guard) AcquireNow() (bool, error) {
	return g.attempt()
}

// Acquire waits for the claim, giving up only when ctx is done. A ctx that is
// cancelled reports ctx.Err(); a ctx whose deadline passes reports
// [ErrWaitExpired].
func (g *Guard) Acquire(ctx context.Context) error {
	return g.wait(ctx, 0)
}

// AcquireWithin waits up to limit for the claim, and reports [ErrWaitExpired]
// if that is not long enough. A limit that is not positive means a single
// attempt: if the claim is not free this instant, the wait has already expired.
//
// The wait also ends if ctx does, reporting as [Guard.Acquire] does.
func (g *Guard) AcquireWithin(ctx context.Context, limit time.Duration) error {
	if limit <= 0 {
		ok, err := g.attempt()
		if err != nil {
			return err
		}
		if !ok {
			return g.expired()
		}
		return nil
	}
	return g.wait(ctx, limit)
}

// Release gives the claim back. It reports [ErrNotHolding] if this Guard does
// not hold one.
func (g *Guard) Release() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return g.errf(ErrGuardClosed)
	}
	if !g.holding {
		return g.errf(ErrNotHolding)
	}
	return g.release()
}

// Close releases the claim if this Guard holds one and closes the underlying
// file handle. The lock file itself is left in place. Closing twice is
// harmless, and a closed Guard reports [ErrGuardClosed] from every other
// method.
func (g *Guard) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true

	var err error
	if g.holding {
		err = g.release()
	}
	if cerr := sysClose(g.h); err == nil {
		err = cerr
	}
	return err
}

// attempt makes one non-blocking try for the claim.
func (g *Guard) attempt() (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.closed {
		return false, g.errf(ErrGuardClosed)
	}
	if g.holding {
		return false, g.errf(ErrAlreadyHolding)
	}

	// The in-process slot comes first, so that a filesystem whose locks are
	// owned by the process rather than by the handle cannot let a second
	// Guard here think it won. See inproc.go.
	if !claimSlot(g.id) {
		return false, nil
	}
	ok, err := sysAcquire(g.h, g.path)
	if err != nil || !ok {
		freeSlot(g.id)
		return false, err
	}
	g.holding = true

	if g.cfg.stamp {
		// Diagnostic only: a claim that is held but unlabelled is far
		// better than an acquire that fails because a comment could not
		// be written.
		_ = writeHolder(g.h)
	}
	return true, nil
}

// wait retries until the claim is taken, ctx is done, or limit elapses. A
// limit that is not positive leaves the wait bounded only by ctx.
func (g *Guard) wait(ctx context.Context, limit time.Duration) error {
	var deadline time.Time
	if limit > 0 {
		deadline = time.Now().Add(limit)
	}

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	retry := g.cfg.minRetry
	for {
		if err := ctx.Err(); err != nil {
			return g.fromContext(err)
		}

		ok, err := g.attempt()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		pause := jitter(retry)
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return g.expired()
			}
			if pause > remaining {
				pause = remaining
			}
		}

		if timer == nil {
			timer = time.NewTimer(pause)
		} else {
			timer.Reset(pause)
		}
		select {
		case <-ctx.Done():
			return g.fromContext(ctx.Err())
		case <-timer.C:
		}

		if retry < g.cfg.maxRetry {
			if retry *= 2; retry > g.cfg.maxRetry {
				retry = g.cfg.maxRetry
			}
		}
	}
}

// release drops the claim. The caller holds g.mu and has checked the state.
func (g *Guard) release() error {
	err := sysRelease(g.h, g.path)
	// The claim is gone either way: a failed release leaves a handle that is
	// no longer trustworthy, and closing it drops the OS claim regardless.
	g.holding = false
	freeSlot(g.id)
	return err
}

// jitter spreads retries out, so that a crowd of waiters released at once does
// not line up on the same schedule and collide on every attempt.
func jitter(d time.Duration) time.Duration {
	spread := int64(d / 4)
	if spread <= 0 {
		return d
	}
	return d - time.Duration(rand.Int64N(spread))
}

// fromContext translates a finished context into this package's vocabulary:
// running out of time is a wait that expired, however the clock got there,
// while a cancellation is the caller's own error coming back.
func (g *Guard) fromContext(err error) error {
	if err == context.DeadlineExceeded {
		return g.expired()
	}
	return fmt.Errorf("filelock: waiting for %s: %w", g.path, err)
}

func (g *Guard) expired() error {
	return fmt.Errorf("%w (%s)", ErrWaitExpired, g.path)
}

func (g *Guard) errf(err error) error {
	return fmt.Errorf("%w (%s)", err, g.path)
}

// Hold opens the lock file at path, waits up to limit for the claim, runs fn
// with it held, and gives it back. It is the whole lifecycle for callers that
// need the claim once. A limit that is not positive means a single attempt,
// as in [Guard.AcquireWithin].
func Hold(ctx context.Context, path string, limit time.Duration, fn func() error, opts ...Option) (err error) {
	g, err := Open(path, opts...)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := g.Close(); err == nil {
			err = cerr
		}
	}()
	if err = g.AcquireWithin(ctx, limit); err != nil {
		return err
	}
	return fn()
}
