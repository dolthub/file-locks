// Copyright 2026 Dolthub, Inc. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the root of this repository.

//go:build windows

package fslock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// LockFileEx byte-range locks belong to the handle, not to the process, so
// two Locks in one process exclude each other the same way two processes do,
// and Windows drops the lock when the process dies.
//
// The lock is always taken with LOCKFILE_FAIL_IMMEDIATELY, and waiting is the
// caller's retry loop. LockFileEx reports completion through the OVERLAPPED's
// event only when the file was opened for overlapped I/O; on an ordinary
// handle it blocks until the lock is granted, deadline or no deadline.

// The locked range is one byte past anything a lock file will hold.
const lockOffsetHigh = 0x80000000

func region() *windows.Overlapped { return &windows.Overlapped{OffsetHigh: lockOffsetHigh} }

func sysLock(f *os.File) (bool, error) {
	var locked bool
	var lockErr error
	err := control(f, func(fd uintptr) {
		flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
		switch err := windows.LockFileEx(windows.Handle(fd), flags, 0, 1, 0, region()); {
		case err == nil:
			locked = true
		case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
			// Someone else holds it.
		default:
			lockErr = err
		}
	})
	if err != nil {
		return false, err
	}
	return locked, lockErr
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
