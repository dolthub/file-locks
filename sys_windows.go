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

//go:build windows

package fslock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// On windows the lock is a LockFileEx byte-range lock.
//
// Byte-range locks belong to the handle they were taken through, which gives
// the same shape as flock(2) on unix: two Locks in one process each have
// their own handle and exclude each other. Windows drops the lock when the
// handle is closed and when the process dies, so a crashed holder leaves
// nothing behind.

// sysWaitsInKernel is true here: LockFileEx can wait, and unlike flock(2) it
// can be interrupted while it does. The request completes asynchronously
// through an OVERLAPPED, so the wait is a WaitForMultipleObjects over the
// event it signals and a second event this package signals to give up.
const sysWaitsInKernel = true

// The locked range is a single byte far past any content a lock file will
// ever have, so that nothing written to the file overlaps it.
const (
	lockOffsetHigh = 0x80000000
	lockBytes      = 1
)

func region() *windows.Overlapped {
	return &windows.Overlapped{OffsetHigh: lockOffsetHigh}
}

// sysAcquire takes the lock, waiting for it when block is set, until ctx is
// done or deadline passes.
func sysAcquire(f *os.File, block bool, ctx context.Context, deadline time.Time) (bool, error) {
	var locked bool
	var lockErr error
	err := control(f, func(fd uintptr) {
		h := windows.Handle(fd)
		if block {
			locked, lockErr = lockWait(h, ctx, deadline)
			return
		}
		locked, lockErr = lockNow(h)
	})
	if err != nil {
		return false, err
	}
	return locked, lockErr
}

// lockNow tries once, without waiting.
func lockNow(h windows.Handle) (bool, error) {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	err := windows.LockFileEx(h, flags, 0, lockBytes, 0, region())
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		// Someone else holds it. An answer, not a failure.
		return false, nil
	}
	return false, err
}

// lockWait issues the lock request and waits for it, watching a cancellation
// event alongside so that a context or a deadline can end the wait.
func lockWait(h windows.Handle, ctx context.Context, deadline time.Time) (bool, error) {
	// The kernel signals this event when the lock request completes.
	granted, err := windows.CreateEvent(nil, 1 /* manual reset */, 0 /* unsignalled */, nil)
	if err != nil {
		return false, fmt.Errorf("CreateEvent: %w", err)
	}
	defer windows.CloseHandle(granted)

	// And this one is ours, to break the wait when the caller gives up.
	// Nothing else can reach a thread inside WaitForMultipleObjects.
	abort, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return false, fmt.Errorf("CreateEvent: %w", err)
	}
	defer windows.CloseHandle(abort)

	if ctx.Done() != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				windows.SetEvent(abort)
			case <-stop:
			}
		}()
	}

	ol := region()
	ol.HEvent = granted

	switch err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, lockBytes, 0, ol); {
	case err == nil:
		// Nobody had it; the request completed on the spot.
		return true, nil
	case !errors.Is(err, windows.ERROR_IO_PENDING):
		return false, err
	}

	// From here the kernel owns ol and granted until the request completes
	// or is cancelled, so every path below waits for one of those.
	event, err := windows.WaitForMultipleObjects(
		[]windows.Handle{granted, abort}, false /* any */, waitMillis(deadline))
	if err != nil {
		discard(h, ol)
		return false, fmt.Errorf("WaitForMultipleObjects: %w", err)
	}

	switch event {
	case windows.WAIT_OBJECT_0:
		var n uint32
		if err := windows.GetOverlappedResult(h, ol, &n, false); err != nil {
			return false, err
		}
		return true, nil
	case windows.WAIT_OBJECT_0 + 1, uint32(windows.WAIT_TIMEOUT):
		return abandon(h, ol)
	default:
		discard(h, ol)
		return false, fmt.Errorf("fslock: unexpected wait result %#x", event)
	}
}

// abandon withdraws a lock request the caller no longer wants, and reports
// whether the lock was granted anyway.
func abandon(h windows.Handle, ol *windows.Overlapped) (bool, error) {
	if err := windows.CancelIoEx(h, ol); err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
		return false, err
	}
	// Wait for the request to settle either way. The OVERLAPPED and its
	// event are the kernel's until it does, and both are about to go out
	// of scope here.
	var n uint32
	switch err := windows.GetOverlappedResult(h, ol, &n, true /* wait */); {
	case err == nil:
		// Granted in the moment we were giving up. Keep it: the caller
		// asked for this lock, and letting it go now would only mean
		// taking it again later.
		return true, nil
	case errors.Is(err, windows.ERROR_OPERATION_ABORTED):
		return false, nil
	default:
		return false, err
	}
}

// discard withdraws a request whose outcome we are about to report as a
// failure. If the lock turns out to have been granted on the way past, it is
// released: a lock nobody knows they hold is held until the process exits.
func discard(h windows.Handle, ol *windows.Overlapped) {
	if granted, _ := abandon(h, ol); granted {
		windows.UnlockFileEx(h, 0, lockBytes, 0, region())
	}
}

// waitMillis converts a deadline into a WaitForMultipleObjects timeout.
func waitMillis(deadline time.Time) uint32 {
	if deadline.IsZero() {
		return windows.INFINITE
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0
	}
	ms := remaining.Milliseconds()
	if ms >= int64(windows.INFINITE) {
		return windows.INFINITE - 1
	}
	return uint32(ms)
}

// sysRelease drops the lock.
func sysRelease(f *os.File) error {
	var unlockErr error
	err := control(f, func(fd uintptr) {
		unlockErr = windows.UnlockFileEx(windows.Handle(fd), 0, lockBytes, 0, region())
	})
	if err != nil {
		return err
	}
	return unlockErr
}

// sysIdentify returns the filesystem's identity for the open file.
func sysIdentify(f *os.File) (fileID, error) {
	var id fileID
	var infoErr error
	err := control(f, func(fd uintptr) {
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(windows.Handle(fd), &info); err != nil {
			infoErr = err
			return
		}
		id = fileID{
			dev: uint64(info.VolumeSerialNumber),
			ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		}
	})
	if err != nil {
		return fileID{}, err
	}
	return id, infoErr
}
