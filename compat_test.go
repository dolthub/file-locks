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

package fslock_test

// These tests are the call shapes this package exists to be dropped into,
// written the way the calling code writes them. They are here so that a
// change to this package that would need an edit over there fails a test
// here first.

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	fslock "github.com/dolthub/file-locks"
)

// New, TryLock, Unlock, Close - and the lock released before the handle.
func TestShapeTryLockAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LOCK")

	lck, err := fslock.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lck.TryLock(); err != nil {
		lck.Close()
		t.Fatal(err)
	}

	err = lck.Unlock()
	if cerr := lck.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TryLock compared against the sentinel with ==, not errors.Is.
func TestShapeEqualsErrLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LOCK")

	held, err := fslock.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := held.Lock(); err != nil {
		t.Fatal(err)
	}

	other, err := fslock.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	err = other.TryLock()
	if err != fslock.ErrLocked {
		t.Fatalf("TryLock = %v, want the ErrLocked value itself", err)
	}
}

// A short bounded wait, with the timeout told apart from a real failure.
func TestShapeLockWithTimeout(t *testing.T) {
	const lockFileTimeout = 100 * time.Millisecond
	path := filepath.Join(t.TempDir(), "LOCK")

	held, err := fslock.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := held.Lock(); err != nil {
		t.Fatal(err)
	}

	lck, err := fslock.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lck.Close()

	err = lck.LockWithTimeout(lockFileTimeout)
	if !errors.Is(err, fslock.ErrTimeout) {
		t.Fatalf("LockWithTimeout = %v, want ErrTimeout", err)
	}
	// Which the caller turns into its own message, keeping the cause.
	wrapped := errors.Join(errors.New("timed out reading database manifest"), err)
	if !errors.Is(wrapped, fslock.ErrTimeout) {
		t.Error("a wrapped timeout must still match ErrTimeout")
	}
}

// LockWithTimeout(0) as "do not wait at all".
func TestShapeLockWithTimeoutZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LOCK")

	held, err := fslock.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := held.Lock(); err != nil {
		t.Fatal(err)
	}

	lck, err := fslock.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lck.Close()

	if err := lck.LockWithTimeout(0); err == nil {
		t.Fatal("LockWithTimeout(0) took a lock somebody else holds")
	}
}

// Take the lock for the lifetime of a handle, fall back to read-only when it
// is not available, and hold it open until Close.
func TestShapeExclusiveOrReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LOCK")

	openStore := func(timeout time.Duration) (exclusive bool, cleanup func(), err error) {
		lck, err := fslock.New(path)
		if err != nil {
			return false, nil, err
		}
		if timeout == 0 {
			err = lck.TryLock()
			if errors.Is(err, fslock.ErrLocked) {
				err = fslock.ErrTimeout
			}
		} else {
			err = lck.LockWithTimeout(timeout)
		}
		if errors.Is(err, fslock.ErrTimeout) {
			lck.Close()
			return false, func() {}, nil // read-only, no lock held
		} else if err != nil {
			lck.Close()
			return false, nil, err
		}
		return true, func() { lck.Unlock(); lck.Close() }, nil
	}

	exclusive, closeFirst, err := openStore(100 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !exclusive {
		t.Fatal("the first opener should have exclusive access")
	}

	exclusive, closeSecond, err := openStore(100 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if exclusive {
		t.Fatal("the second opener should have fallen back to read-only")
	}
	closeSecond()
	closeFirst()

	exclusive, closeThird, err := openStore(0)
	if err != nil {
		t.Fatal(err)
	}
	if !exclusive {
		t.Fatal("the lock should be free once the first opener closed")
	}
	closeThird()
}
