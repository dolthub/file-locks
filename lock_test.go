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

const holdEnv = "FSLOCK_TEST_HOLD"

// With FSLOCK_TEST_HOLD set, this binary is the second process the last test
// needs: it takes the named lock, says so, and waits to be killed.
func TestMain(m *testing.M) {
	path := os.Getenv(holdEnv)
	if path == "" {
		os.Exit(m.Run())
	}
	lck, err := New(path)
	if err != nil {
		os.Exit(2)
	}
	if err := lck.Lock(); err != nil {
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

// A lock on a file that has been unlinked excludes nobody, so taking one must
// not report success.
func TestReplacedLockFile(t *testing.T) {
	path := tempPath(t)
	stale := newLock(t, path)

	if err := os.Remove(path); err != nil {
		t.Skipf("this platform will not remove an open lock file: %v", err)
	}
	replacement := newLock(t, path)
	if err := replacement.TryLock(); err != nil {
		t.Fatal(err)
	}
	if err := stale.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}
}

func TestAnotherProcess(t *testing.T) {
	path := tempPath(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(self)
	child.Env = append(os.Environ(), holdEnv+"="+path)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()

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

	lck := newLock(t, path)
	if err := lck.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}

	// Killing the holder releases the lock; nothing here cleans up after it.
	child.Process.Kill()
	child.Wait()
	if err := lck.LockWithTimeout(10 * time.Second); err != nil {
		t.Fatal(err)
	}
}
