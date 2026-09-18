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
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// openTest opens a Guard that is closed when the test ends.
func openTest(t *testing.T, path string, opts ...Option) *Guard {
	t.Helper()
	g, err := Open(path, opts...)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "LOCK")
}

func acquireNow(t *testing.T, g *Guard) bool {
	t.Helper()
	ok, err := g.AcquireNow()
	if err != nil {
		t.Fatalf("AcquireNow: %v", err)
	}
	return ok
}

func TestOpenCreatesLockFileAndLeavesIt(t *testing.T) {
	path := lockPath(t)

	g, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file was not created: %v", err)
	}
	if g.Path() != path {
		t.Errorf("Path() = %q, want %q", g.Path(), path)
	}
	if g.Holding() {
		t.Error("a freshly opened guard must not hold the claim")
	}
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A closed guard must not take the lock file with it: another
	// participant has to be able to open the same file afterwards.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file was removed by Close: %v", err)
	}
	again := openTest(t, path)
	if !acquireNow(t, again) {
		t.Error("could not claim a lock file left behind by a closed guard")
	}
}

func TestOpenMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no", "such", "dir", "LOCK")
	if g, err := Open(path); err == nil {
		g.Close()
		t.Fatal("Open must fail when the directory does not exist")
	}
}

func TestAcquireNowExcludesAnotherGuard(t *testing.T) {
	path := lockPath(t)
	first, second := openTest(t, path), openTest(t, path)

	if !acquireNow(t, first) {
		t.Fatal("the first claim on a free lock file must succeed")
	}
	if !first.Holding() {
		t.Error("Holding() must report a held claim")
	}
	if acquireNow(t, second) {
		t.Fatal("a second guard claimed a lock file that is already held")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if first.Holding() {
		t.Error("Holding() must report a released claim")
	}
	if !acquireNow(t, second) {
		t.Fatal("the claim was not free after the holder released it")
	}
}

func TestAcquireWithinExpires(t *testing.T) {
	path := lockPath(t)
	holder, waiter := openTest(t, path), openTest(t, path)
	if !acquireNow(t, holder) {
		t.Fatal("could not take the claim")
	}

	const limit = 40 * time.Millisecond
	start := time.Now()
	err := waiter.AcquireWithin(context.Background(), limit)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("AcquireWithin = %v, want ErrWaitExpired", err)
	}
	// Callers reaching for the standard vocabulary find it too.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("ErrWaitExpired must also report as context.DeadlineExceeded")
	}
	if elapsed < limit {
		t.Errorf("gave up after %v, before the %v limit", elapsed, limit)
	}
	if waiter.Holding() {
		t.Error("an expired wait must not leave the guard holding")
	}
}

func TestAcquireWithinSucceedsWhenTheHolderLeaves(t *testing.T) {
	path := lockPath(t)
	holder, waiter := openTest(t, path), openTest(t, path)
	if !acquireNow(t, holder) {
		t.Fatal("could not take the claim")
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		holder.Release()
	}()

	if err := waiter.AcquireWithin(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("AcquireWithin: %v", err)
	}
	if !waiter.Holding() {
		t.Error("the waiter reported success without holding the claim")
	}
}

func TestAcquireWithinZeroTriesOnce(t *testing.T) {
	path := lockPath(t)
	holder, waiter := openTest(t, path), openTest(t, path)
	if !acquireNow(t, holder) {
		t.Fatal("could not take the claim")
	}

	start := time.Now()
	err := waiter.AcquireWithin(context.Background(), 0)
	if !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("AcquireWithin(0) = %v, want ErrWaitExpired", err)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
		t.Errorf("AcquireWithin(0) waited %v; it must try once and return", elapsed)
	}

	if err := holder.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := waiter.AcquireWithin(context.Background(), 0); err != nil {
		t.Fatalf("AcquireWithin(0) on a free claim: %v", err)
	}
}

func TestAcquireWaitsUntilCancelled(t *testing.T) {
	path := lockPath(t)
	holder, waiter := openTest(t, path), openTest(t, path)
	if !acquireNow(t, holder) {
		t.Fatal("could not take the claim")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	err := waiter.Acquire(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrWaitExpired) {
		t.Error("a cancellation is not an expired wait")
	}
}

func TestAcquireHonorsContextDeadline(t *testing.T) {
	path := lockPath(t)
	holder, waiter := openTest(t, path), openTest(t, path)
	if !acquireNow(t, holder) {
		t.Fatal("could not take the claim")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	if err := waiter.Acquire(ctx); !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("Acquire = %v, want ErrWaitExpired", err)
	}
}

func TestAcquireOnCancelledContext(t *testing.T) {
	g := openTest(t, lockPath(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := g.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
	if g.Holding() {
		t.Error("a cancelled Acquire must not take the claim")
	}
}

// One guard is reused for the lifetime of a store, taking and giving back the
// claim around every update.
func TestRepeatedCycles(t *testing.T) {
	path := lockPath(t)
	g := openTest(t, path)
	other := openTest(t, path)

	for i := 0; i < 100; i++ {
		if err := g.AcquireWithin(context.Background(), time.Second); err != nil {
			t.Fatalf("cycle %d: acquire: %v", i, err)
		}
		if acquireNow(t, other) {
			t.Fatalf("cycle %d: the claim was not exclusive", i)
		}
		if err := g.Release(); err != nil {
			t.Fatalf("cycle %d: release: %v", i, err)
		}
	}
}

func TestAlreadyHolding(t *testing.T) {
	g := openTest(t, lockPath(t))
	if !acquireNow(t, g) {
		t.Fatal("could not take the claim")
	}

	if _, err := g.AcquireNow(); !errors.Is(err, ErrAlreadyHolding) {
		t.Errorf("AcquireNow = %v, want ErrAlreadyHolding", err)
	}
	if err := g.AcquireWithin(context.Background(), time.Millisecond); !errors.Is(err, ErrAlreadyHolding) {
		t.Errorf("AcquireWithin = %v, want ErrAlreadyHolding", err)
	}
	if err := g.Acquire(context.Background()); !errors.Is(err, ErrAlreadyHolding) {
		t.Errorf("Acquire = %v, want ErrAlreadyHolding", err)
	}
	if !g.Holding() {
		t.Error("a refused re-acquire must leave the original claim alone")
	}
}

func TestReleaseWithoutHolding(t *testing.T) {
	g := openTest(t, lockPath(t))
	if err := g.Release(); !errors.Is(err, ErrNotHolding) {
		t.Errorf("Release = %v, want ErrNotHolding", err)
	}

	if !acquireNow(t, g) {
		t.Fatal("could not take the claim")
	}
	if err := g.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := g.Release(); !errors.Is(err, ErrNotHolding) {
		t.Errorf("second Release = %v, want ErrNotHolding", err)
	}
}

func TestCloseReleasesAndIsFinal(t *testing.T) {
	path := lockPath(t)
	g, other := openTest(t, path), openTest(t, path)
	if !acquireNow(t, g) {
		t.Fatal("could not take the claim")
	}

	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Errorf("closing twice must be harmless, got %v", err)
	}
	if !acquireNow(t, other) {
		t.Fatal("Close did not give the claim back")
	}

	if _, err := g.AcquireNow(); !errors.Is(err, ErrGuardClosed) {
		t.Errorf("AcquireNow after Close = %v, want ErrGuardClosed", err)
	}
	if err := g.Acquire(context.Background()); !errors.Is(err, ErrGuardClosed) {
		t.Errorf("Acquire after Close = %v, want ErrGuardClosed", err)
	}
	if err := g.Release(); !errors.Is(err, ErrGuardClosed) {
		t.Errorf("Release after Close = %v, want ErrGuardClosed", err)
	}
}

// Errors name the lock file, so that a message reaching a person says which
// one it was about.
func TestErrorsNameTheFile(t *testing.T) {
	path := lockPath(t)
	holder, waiter := openTest(t, path), openTest(t, path)
	if !acquireNow(t, holder) {
		t.Fatal("could not take the claim")
	}

	err := waiter.AcquireWithin(context.Background(), time.Millisecond)
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

// Guards opened through different spellings of one path claim the same file.
func TestSameFileThroughDifferentPaths(t *testing.T) {
	dir := t.TempDir()
	direct := openTest(t, filepath.Join(dir, "LOCK"))
	indirect := openTest(t, filepath.Join(dir, ".", "sub", "..", "LOCK"))

	if !acquireNow(t, direct) {
		t.Fatal("could not take the claim")
	}
	if acquireNow(t, indirect) {
		t.Fatal("two spellings of one path were treated as two lock files")
	}
}

// The claim serializes read-modify-write on a file it guards, across
// goroutines using independent guards. Run under -race.
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
			g, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			defer g.Close()

			for i := 0; i < each; i++ {
				if err := g.Acquire(context.Background()); err != nil {
					errs <- err
					return
				}
				if err := bump(counter); err != nil {
					g.Release()
					errs <- err
					return
				}
				if err := g.Release(); err != nil {
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

func TestHold(t *testing.T) {
	path := lockPath(t)

	ran := false
	if err := Hold(context.Background(), path, time.Second, func() error {
		ran = true
		g := openTest(t, path)
		if acquireNow(t, g) {
			t.Error("the claim was not held while fn ran")
		}
		return nil
	}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if !ran {
		t.Fatal("Hold did not run fn")
	}

	// The claim is given back when fn returns.
	after := openTest(t, path)
	if !acquireNow(t, after) {
		t.Error("Hold did not release the claim")
	}
}

func TestHoldReportsFnError(t *testing.T) {
	want := errors.New("boom")
	err := Hold(context.Background(), lockPath(t), time.Second, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("Hold = %v, want %v", err, want)
	}
}

func TestHoldWaitsOutABusyClaim(t *testing.T) {
	path := lockPath(t)
	holder := openTest(t, path)
	if !acquireNow(t, holder) {
		t.Fatal("could not take the claim")
	}

	err := Hold(context.Background(), path, 20*time.Millisecond, func() error {
		t.Error("fn must not run without the claim")
		return nil
	})
	if !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("Hold = %v, want ErrWaitExpired", err)
	}
}

func TestHolderStamp(t *testing.T) {
	path := lockPath(t)
	g := openTest(t, path, WithHolderStamp())

	if _, err := g.ReadHolder(); !errors.Is(err, ErrNoHolderStamp) {
		t.Errorf("ReadHolder before any claim = %v, want ErrNoHolderStamp", err)
	}

	before := time.Now().Add(-time.Second)
	if !acquireNow(t, g) {
		t.Fatal("could not take the claim")
	}

	h, err := g.ReadHolder()
	if err != nil {
		t.Fatalf("ReadHolder: %v", err)
	}
	if h.PID != os.Getpid() {
		t.Errorf("stamped pid = %d, want %d", h.PID, os.Getpid())
	}
	if h.Since.Before(before) {
		t.Errorf("stamped time %v is not from this acquire", h.Since)
	}
	if h.String() == "" {
		t.Error("Holder.String() is empty")
	}

	// A guard that did not ask for a stamp reads one another guard wrote:
	// the stamp is a property of the file, not of the reader.
	plain := openTest(t, path)
	if _, err := plain.ReadHolder(); err != nil {
		t.Errorf("ReadHolder from an unstamped guard: %v", err)
	}
}

func TestNoHolderStampByDefault(t *testing.T) {
	path := lockPath(t)
	g := openTest(t, path)
	if !acquireNow(t, g) {
		t.Fatal("could not take the claim")
	}
	if _, err := g.ReadHolder(); !errors.Is(err, ErrNoHolderStamp) {
		t.Errorf("ReadHolder = %v, want ErrNoHolderStamp: stamping is opt-in", err)
	}
}

func TestWithRetrySchedule(t *testing.T) {
	path := lockPath(t)
	holder := openTest(t, path)
	if !acquireNow(t, holder) {
		t.Fatal("could not take the claim")
	}

	// A schedule coarser than the wait still expires on time rather than
	// overshooting by a whole retry interval.
	waiter := openTest(t, path, WithRetrySchedule(80*time.Millisecond, 80*time.Millisecond))
	start := time.Now()
	if err := waiter.AcquireWithin(context.Background(), 20*time.Millisecond); !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("AcquireWithin = %v, want ErrWaitExpired", err)
	}
	if elapsed := time.Since(start); elapsed > 70*time.Millisecond {
		t.Errorf("waited %v for a 20ms limit", elapsed)
	}
}

func BenchmarkUncontendedCycle(b *testing.B) {
	dir := b.TempDir()
	g, err := Open(filepath.Join(dir, "LOCK"))
	if err != nil {
		b.Fatal(err)
	}
	defer g.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.AcquireNow(); err != nil {
			b.Fatal(err)
		}
		if err := g.Release(); err != nil {
			b.Fatal(err)
		}
	}
}
