// Copyright 2026 Dolthub, Inc. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the root of this repository.

package fslock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	holdEnv       = "FSLOCK_TEST_HOLD"
	holdSharedEnv = "FSLOCK_TEST_HOLD_SHARED"
)

// With FSLOCK_TEST_HOLD set, this binary is the second process the last tests
// need: it takes the named lock, says so, and waits to be killed. With
// FSLOCK_TEST_HOLD_SHARED set as well, it takes the lock in shared mode.
func TestMain(m *testing.M) {
	path := os.Getenv(holdEnv)
	if path == "" {
		os.Exit(m.Run())
	}
	lck, err := New(path)
	if err != nil {
		os.Exit(2)
	}
	take := lck.Lock
	if os.Getenv(holdSharedEnv) != "" {
		take = lck.RLock
	}
	if err := take(); err != nil {
		os.Exit(2)
	}
	os.WriteFile(path+".held", nil, 0666)
	time.Sleep(time.Minute)
	os.Exit(0)
}

func tempPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "LOCK")
}

func newLock(t *testing.T, path string) *Lock {
	t.Helper()
	lck, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lck.Close() })
	return lck
}

// within runs fn, failing the test if it does not return soon. A wait that
// ignores its deadline should fail a test, not hang it.
func within(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the call did not return")
		return nil
	}
}

func TestExclusion(t *testing.T) {
	path := tempPath(t)
	a, b := newLock(t, path), newLock(t, path)

	if err := a.TryLock(); err != nil {
		t.Fatal(err)
	}
	if err := b.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := b.TryLock(); err != nil {
		t.Fatal(err)
	}
}

func TestTimeout(t *testing.T) {
	path := tempPath(t)
	holder, waiter := newLock(t, path), newLock(t, path)
	if err := holder.Lock(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err := within(t, func() error { return waiter.LockWithTimeout(40 * time.Millisecond) })
	if err != ErrTimeout {
		t.Fatalf("LockWithTimeout = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("gave up after %v, before the limit", elapsed)
	}
	if !IsTimeout(err) || !IsTemporary(err) {
		t.Error("a timeout is both a timeout and temporary")
	}
	if !IsTemporary(ErrLocked) || IsTimeout(ErrLocked) {
		t.Error("ErrLocked is temporary but not a timeout")
	}

	// LockWithTimeout(0) is one attempt.
	if err := waiter.LockWithTimeout(0); err != ErrTimeout {
		t.Fatalf("LockWithTimeout(0) = %v, want ErrTimeout", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		holder.Unlock()
	}()
	if err := waiter.LockWithTimeout(10 * time.Second); err != nil {
		t.Fatal(err)
	}
}

// An unbounded Lock waits in the kernel, where waiters queue.
func TestBlockingLock(t *testing.T) {
	path := tempPath(t)
	holder, waiter := newLock(t, path), newLock(t, path)
	if err := holder.Lock(); err != nil {
		t.Fatal(err)
	}

	const hold = 50 * time.Millisecond
	go func() {
		time.Sleep(hold)
		holder.Unlock()
	}()

	start := time.Now()
	if err := within(t, waiter.Lock); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < hold {
		t.Errorf("Lock returned after %v, before the holder let go", elapsed)
	}
}

func TestContext(t *testing.T) {
	path := tempPath(t)
	holder, waiter := newLock(t, path), newLock(t, path)
	if err := holder.Lock(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := within(t, func() error { return waiter.LockWithContext(ctx) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LockWithContext = %v, want context.Canceled", err)
	}
	if IsTemporary(err) {
		t.Error("a cancellation is not temporary")
	}
}

func TestShared(t *testing.T) {
	path := tempPath(t)
	a, b, c := newLock(t, path), newLock(t, path), newLock(t, path)

	if err := a.TryRLock(); err != nil {
		t.Fatal(err)
	}
	if err := b.RLockWithTimeout(0); err != nil {
		t.Fatalf("a shared holder must not block another: %v", err)
	}
	if err := c.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}
	if err := c.LockWithTimeout(0); err != ErrTimeout {
		t.Fatalf("LockWithTimeout(0) = %v, want ErrTimeout", err)
	}

	// The exclusive lock has to wait for every shared holder.
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := c.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock with one holder left = %v, want ErrLocked", err)
	}
	if err := b.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := c.TryLock(); err != nil {
		t.Fatal(err)
	}

	// And an exclusive holder keeps shared holders out.
	if err := a.TryRLock(); err != ErrLocked {
		t.Fatalf("TryRLock = %v, want ErrLocked", err)
	}
	if err := a.RLockWithTimeout(0); err != ErrTimeout {
		t.Fatalf("RLockWithTimeout(0) = %v, want ErrTimeout", err)
	}
	start := time.Now()
	err := within(t, func() error { return a.RLockWithTimeout(40 * time.Millisecond) })
	if err != ErrTimeout {
		t.Fatalf("RLockWithTimeout = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("gave up after %v, before the limit", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err = within(t, func() error { return a.RLockWithContext(ctx) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RLockWithContext = %v, want context.Canceled", err)
	}

	// An unbounded RLock waits for the exclusive holder, then gets in.
	const hold = 50 * time.Millisecond
	go func() {
		time.Sleep(hold)
		c.Unlock()
	}()
	start = time.Now()
	if err := within(t, a.RLock); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < hold {
		t.Errorf("RLock returned after %v, before the holder let go", elapsed)
	}
	if err := within(t, b.RLock); err != nil {
		t.Fatalf("a shared holder must not block an unbounded RLock: %v", err)
	}
}

func TestSharedMisuseAndClose(t *testing.T) {
	path := tempPath(t)
	a, b := newLock(t, path), newLock(t, path)

	if err := a.RLock(); err != nil {
		t.Fatal(err)
	}
	if err := a.RLock(); err == nil {
		t.Error("locking twice must fail")
	}
	// A Lock cannot change mode: flock(2) would do it by letting go first.
	if err := a.Lock(); err == nil {
		t.Error("taking the exclusive lock while holding the shared lock must fail")
	}
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := a.RLock(); err == nil {
		t.Error("taking the shared lock while holding the exclusive lock must fail")
	}
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}

	if err := a.RLock(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.TryRLock(); err == nil {
		t.Error("a closed Lock must fail")
	}
	if err := b.TryLock(); err != nil {
		t.Fatalf("Close did not release the shared lock: %v", err)
	}
}

func TestCloseAndMisuse(t *testing.T) {
	path := tempPath(t)
	a, b := newLock(t, path), newLock(t, path)

	if err := a.Unlock(); err == nil {
		t.Error("unlocking a lock we do not hold must fail")
	}
	if err := a.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(); err == nil {
		t.Error("locking twice must fail")
	}

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("closing twice must be harmless: %v", err)
	}
	if err := a.TryLock(); err == nil {
		t.Error("a closed Lock must fail")
	}
	if err := b.TryLock(); err != nil {
		t.Fatalf("Close did not release the lock: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the lock file was removed: %v", err)
	}
}

// A lock file has to stay deletable while it is held. The directory it sits
// in belongs to the caller, who may clear it up at any time.
func TestRemoveWhileHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "LOCK")
	lck := newLock(t, path)
	if err := lck.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing a held lock file: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("removing the directory of a held lock file: %v", err)
	}
}

// A lock on a file that has been unlinked excludes nobody, so taking one must
// not report success.
func TestReplacedLockFile(t *testing.T) {
	path := tempPath(t)
	stale := newLock(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatalf("removing a held lock file: %v", err)
	}
	replacement := newLock(t, path)
	if err := replacement.TryLock(); err != nil {
		t.Fatal(err)
	}
	if err := stale.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}
	if err := stale.TryRLock(); err != ErrLocked {
		t.Fatalf("TryRLock = %v, want ErrLocked", err)
	}
}

// startHolder starts a second process which holds the lock at path. It is
// killed at the end of the test if the test has not done so already.
func startHolder(t *testing.T, path string, shared bool) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(self)
	child.Env = append(os.Environ(), holdEnv+"="+path)
	if shared {
		child.Env = append(child.Env, holdSharedEnv+"=1")
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path + ".held"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never took the lock")
		}
		time.Sleep(time.Millisecond)
	}
	return child
}

func TestAnotherProcess(t *testing.T) {
	path := tempPath(t)
	child := startHolder(t, path, false)

	lck := newLock(t, path)
	if err := lck.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}
	if err := lck.TryRLock(); err != ErrLocked {
		t.Fatalf("TryRLock = %v, want ErrLocked", err)
	}

	// Killing the holder releases the lock; nothing here cleans up after it.
	child.Process.Kill()
	child.Wait()
	if err := lck.LockWithTimeout(10 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestAnotherProcessShared(t *testing.T) {
	path := tempPath(t)
	child := startHolder(t, path, true)

	lck := newLock(t, path)
	if err := lck.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}
	if err := lck.TryRLock(); err != nil {
		t.Fatalf("a shared holder in another process must not block TryRLock: %v", err)
	}
	if err := lck.Unlock(); err != nil {
		t.Fatal(err)
	}

	// Killing the holder releases the lock; nothing here cleans up after it.
	child.Process.Kill()
	child.Wait()
	if err := lck.LockWithTimeout(10 * time.Second); err != nil {
		t.Fatal(err)
	}
}
