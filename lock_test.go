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
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// newTest returns a Lock that is closed when the test ends.
func newTest(t *testing.T, path string) *Lock {
	t.Helper()
	lck, err := New(path)
	if err != nil {
		t.Fatalf("New(%s): %v", path, err)
	}
	t.Cleanup(func() { lck.Close() })
	return lck
}

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "LOCK")
}

// held returns a Lock that holds the lock on path.
func held(t *testing.T, path string) *Lock {
	t.Helper()
	lck := newTest(t, path)
	if err := lck.TryLock(); err != nil {
		t.Fatalf("TryLock on a free lock file: %v", err)
	}
	return lck
}

func TestNewCreatesLockFileAndLeavesIt(t *testing.T) {
	path := lockPath(t)

	lck, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the lock file was not created: %v", err)
	}
	if err := lck.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A closed Lock must not take the lock file with it.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the lock file was removed by Close: %v", err)
	}
	if err := newTest(t, path).TryLock(); err != nil {
		t.Errorf("TryLock on a lock file left behind by a closed Lock: %v", err)
	}
}

func TestNewMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no", "such", "dir", "LOCK")
	lck, err := New(path)
	if err == nil {
		lck.Close()
		t.Fatal("New must fail when the directory does not exist")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("New = %v, want it to wrap os.ErrNotExist", err)
	}
	if IsTemporary(err) {
		t.Error("a missing directory is not a temporary failure")
	}
}

func TestTryLockExcludesAnotherLock(t *testing.T) {
	path := lockPath(t)
	first, second := newTest(t, path), newTest(t, path)

	if err := first.TryLock(); err != nil {
		t.Fatalf("TryLock on a free lock file: %v", err)
	}
	if err := second.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want exactly ErrLocked", err)
	}

	if err := first.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := second.TryLock(); err != nil {
		t.Fatalf("TryLock after the holder unlocked: %v", err)
	}
}

func TestLockWithTimeoutExpires(t *testing.T) {
	path := lockPath(t)
	held(t, path)
	waiter := newTest(t, path)

	const limit = 40 * time.Millisecond
	start := time.Now()
	err := waiter.LockWithTimeout(limit)
	elapsed := time.Since(start)

	if err != ErrTimeout {
		t.Fatalf("LockWithTimeout = %v, want exactly ErrTimeout", err)
	}
	if elapsed < limit {
		t.Errorf("gave up after %v, before the %v limit", elapsed, limit)
	}
}

func TestLockWithTimeoutSucceedsWhenTheHolderLeaves(t *testing.T) {
	path := lockPath(t)
	holder := held(t, path)
	waiter := newTest(t, path)

	go func() {
		time.Sleep(30 * time.Millisecond)
		holder.Unlock()
	}()

	if err := waiter.LockWithTimeout(10 * time.Second); err != nil {
		t.Fatalf("LockWithTimeout: %v", err)
	}
}

// LockWithTimeout(0) is how a caller asks for one attempt.
func TestLockWithTimeoutZeroTriesOnce(t *testing.T) {
	path := lockPath(t)
	holder := held(t, path)
	waiter := newTest(t, path)

	start := time.Now()
	if err := waiter.LockWithTimeout(0); err != ErrTimeout {
		t.Fatalf("LockWithTimeout(0) = %v, want exactly ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
		t.Errorf("LockWithTimeout(0) waited %v; it must try once and return", elapsed)
	}

	if err := holder.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := waiter.LockWithTimeout(0); err != nil {
		t.Fatalf("LockWithTimeout(0) on a free lock: %v", err)
	}
}

func TestLockWaitsForTheHolder(t *testing.T) {
	path := lockPath(t)
	holder := held(t, path)
	waiter := newTest(t, path)

	release := time.Now().Add(50 * time.Millisecond)
	go func() {
		time.Sleep(time.Until(release))
		holder.Unlock()
	}()

	start := time.Now()
	if err := waiter.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Error("Lock returned before the holder let go")
	}
}

func TestLockWithContextCancelled(t *testing.T) {
	path := lockPath(t)
	held(t, path)
	waiter := newTest(t, path)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	err := waiter.LockWithContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LockWithContext = %v, want it to wrap context.Canceled", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("a cancellation is not a timeout")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != Canceled {
		t.Errorf("LockWithContext = %v, want Kind Canceled", err)
	}
	if IsTimeout(err) || IsTemporary(err) {
		t.Error("a cancellation is neither a timeout nor temporary")
	}
}

func TestLockWithContextDeadline(t *testing.T) {
	path := lockPath(t)
	held(t, path)
	waiter := newTest(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	err := waiter.LockWithContext(ctx)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("LockWithContext = %v, want it to match ErrTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("a deadline that passed must still report context.DeadlineExceeded")
	}
	if !IsTimeout(err) || !IsTemporary(err) {
		t.Error("a deadline is both a timeout and temporary")
	}
}

func TestLockWithContextAlreadyDone(t *testing.T) {
	lck := newTest(t, lockPath(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := lck.LockWithContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("LockWithContext = %v, want context.Canceled", err)
	}
	if err := lck.Unlock(); !errors.Is(err, &Error{Kind: NotHeld}) {
		t.Errorf("a cancelled LockWithContext must not have taken the lock (Unlock = %v)", err)
	}
}

// One Lock is reused for the lifetime of a store, taking and giving back the
// lock around every update.
func TestRepeatedCycles(t *testing.T) {
	path := lockPath(t)
	lck, other := newTest(t, path), newTest(t, path)

	for i := 0; i < 100; i++ {
		if err := lck.LockWithTimeout(time.Second); err != nil {
			t.Fatalf("cycle %d: lock: %v", i, err)
		}
		if err := other.TryLock(); err != ErrLocked {
			t.Fatalf("cycle %d: the lock was not exclusive: %v", i, err)
		}
		if err := lck.Unlock(); err != nil {
			t.Fatalf("cycle %d: unlock: %v", i, err)
		}
	}
}

func TestTakingALockTwice(t *testing.T) {
	lck := held(t, lockPath(t))

	want := &Error{Kind: AlreadyHeld}
	if err := lck.TryLock(); !errors.Is(err, want) {
		t.Errorf("TryLock = %v, want Kind AlreadyHeld", err)
	}
	if err := lck.LockWithTimeout(time.Millisecond); !errors.Is(err, want) {
		t.Errorf("LockWithTimeout = %v, want Kind AlreadyHeld", err)
	}
	if err := lck.Lock(); !errors.Is(err, want) {
		t.Errorf("Lock = %v, want Kind AlreadyHeld", err)
	}
	// Still ours, and still exactly once.
	if err := lck.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestUnlockWithoutHolding(t *testing.T) {
	lck := newTest(t, lockPath(t))
	want := &Error{Kind: NotHeld}

	if err := lck.Unlock(); !errors.Is(err, want) {
		t.Errorf("Unlock = %v, want Kind NotHeld", err)
	}
	if err := lck.TryLock(); err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if err := lck.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := lck.Unlock(); !errors.Is(err, want) {
		t.Errorf("second Unlock = %v, want Kind NotHeld", err)
	}
}

func TestCloseReleasesAndIsFinal(t *testing.T) {
	path := lockPath(t)
	lck, other := newTest(t, path), newTest(t, path)
	if err := lck.TryLock(); err != nil {
		t.Fatalf("TryLock: %v", err)
	}

	if err := lck.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := lck.Close(); err != nil {
		t.Errorf("closing twice must be harmless, got %v", err)
	}
	if err := other.TryLock(); err != nil {
		t.Fatalf("Close did not give the lock back: %v", err)
	}

	want := &Error{Kind: Closed}
	if err := lck.TryLock(); !errors.Is(err, want) {
		t.Errorf("TryLock after Close = %v, want Kind Closed", err)
	}
	if err := lck.Lock(); !errors.Is(err, want) {
		t.Errorf("Lock after Close = %v, want Kind Closed", err)
	}
	if err := lck.Unlock(); !errors.Is(err, want) {
		t.Errorf("Unlock after Close = %v, want Kind Closed", err)
	}
	if !errors.Is(lck.TryLock(), os.ErrClosed) {
		t.Error("a closed Lock should also report fs.ErrClosed")
	}
}

// Errors name the lock file, so that a message reaching a person says which
// one it was about. The two sentinels are the exception: they are values to
// be compared, and carry nothing.
func TestErrorsNameTheFile(t *testing.T) {
	path := lockPath(t)
	lck := newTest(t, path)

	err := lck.Unlock()
	if err == nil || !contains(err.Error(), path) {
		t.Errorf("error %v does not mention %s", err, path)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The lock serializes read-modify-write on a file it guards, across
// goroutines using independent Locks. Run under -race.
func TestConcurrentGoroutines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "LOCK")
	counter := filepath.Join(dir, "counter")
	if err := os.WriteFile(counter, []byte("0"), 0666); err != nil {
		t.Fatal(err)
	}

	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lck, err := New(path)
			if err != nil {
				errs <- err
				return
			}
			defer lck.Close()

			for i := 0; i < each; i++ {
				if err := lck.Lock(); err != nil {
					errs <- err
					return
				}
				if err := bump(counter); err != nil {
					lck.Unlock()
					errs <- err
					return
				}
				if err := lck.Unlock(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("worker: %v", err)
	}

	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	got, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if want := workers * each; got != want {
		t.Errorf("counter = %d, want %d: updates were lost", got, want)
	}
}

// bump is a read-modify-write that loses updates unless it is serialized.
func bump(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(string(data))
	if err != nil {
		return err
	}
	time.Sleep(time.Microsecond)
	return os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0666)
}

func BenchmarkUncontendedCycle(b *testing.B) {
	lck, err := New(filepath.Join(b.TempDir(), "LOCK"))
	if err != nil {
		b.Fatal(err)
	}
	defer lck.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := lck.TryLock(); err != nil {
			b.Fatal(err)
		}
		if err := lck.Unlock(); err != nil {
			b.Fatal(err)
		}
	}
}
