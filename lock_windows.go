// Copyright 2026 Dolthub, Inc. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the root of this repository.

//go:build windows

package fslock

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// LockFileEx byte-range locks belong to the handle, not to the process, so
// two Locks in one process exclude each other the same way two processes do,
// and Windows drops the lock when the process dies.

// sysWaits is true: LockFileEx completes through an OVERLAPPED, so a wait can
// be broken by an event when the caller gives up.
const sysWaits = true

// The locked range is one byte past anything a lock file will hold.
const lockOffsetHigh = 0x80000000

func region() *windows.Overlapped { return &windows.Overlapped{OffsetHigh: lockOffsetHigh} }

func sysLock(f *os.File, wait bool, ctx context.Context, deadline time.Time) (bool, error) {
	var locked bool
	var lockErr error
	err := control(f, func(fd uintptr) {
		locked, lockErr = lockFile(windows.Handle(fd), wait, ctx, deadline)
	})
	if err != nil {
		return false, err
	}
	return locked, lockErr
}

func lockFile(h windows.Handle, wait bool, ctx context.Context, deadline time.Time) (bool, error) {
	if !wait {
		err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, region())
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return false, nil
		}
		return err == nil, err
	}

	granted, err := windows.CreateEvent(nil, 1, 0, nil) // signalled when the request completes
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(granted)
	abort, err := windows.CreateEvent(nil, 1, 0, nil) // signalled when we give up
	if err != nil {
		return false, err
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
	switch err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); {
	case err == nil:
		return true, nil
	case !errors.Is(err, windows.ERROR_IO_PENDING):
		return false, err
	}

	var n uint32
	event, err := windows.WaitForMultipleObjects([]windows.Handle{granted, abort}, false, waitMillis(deadline))
	if err == nil && event == windows.WAIT_OBJECT_0 {
		if err := windows.GetOverlappedResult(h, ol, &n, false); err != nil {
			return false, err
		}
		return true, nil
	}

	// Giving up: withdraw the request, but keep the lock if it was granted
	// on the way past rather than leaving it held by nobody.
	windows.CancelIoEx(h, ol)
	if windows.GetOverlappedResult(h, ol, &n, true) == nil {
		return true, nil
	}
	return false, err
}

func waitMillis(deadline time.Time) uint32 {
	if deadline.IsZero() {
		return windows.INFINITE
	}
	remaining := time.Until(deadline).Milliseconds()
	if remaining <= 0 {
		return 0
	}
	if remaining >= int64(windows.INFINITE) {
		return windows.INFINITE - 1
	}
	return uint32(remaining)
}

func sysUnlock(f *os.File) error {
	var unlockErr error
	err := control(f, func(fd uintptr) {
		unlockErr = windows.UnlockFileEx(windows.Handle(fd), 0, 1, 0, region())
	})
	if err != nil {
		return err
	}
	return unlockErr
}
