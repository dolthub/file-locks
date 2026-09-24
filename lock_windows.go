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
// On an ordinary handle - which is what os.OpenFile returns - LockFileEx
// blocks until the lock is granted, and the OVERLAPPED's event is signalled
// only for handles opened for overlapped I/O. That is what an unbounded wait
// wants and no use at all to a bounded one, which therefore asks for
// LOCKFILE_FAIL_IMMEDIATELY and retries instead.

// The locked range is one byte past anything a lock file will hold.
const lockOffsetHigh = 0x80000000

func region() *windows.Overlapped { return &windows.Overlapped{OffsetHigh: lockOffsetHigh} }

// sysOpen opens the lock file, creating it if it is not there.
//
// This is CreateFile rather than os.OpenFile because a lock file has to stay
// deletable while it is held: os.OpenFile asks for FILE_SHARE_READ and
// FILE_SHARE_WRITE only, and without FILE_SHARE_DELETE nobody can remove the
// lock file, or the directory holding it, until the lock is closed. Go's own
// (*os.Root).OpenFile shares deletion for the same reason.
//
// Nothing is lost by not going through os.OpenFile: handles are not inherited
// unless asked for, so the close-on-exec care it takes is a unix concern.
func sysOpen(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func sysLock(f *os.File, wait bool) (bool, error) {
	var locked bool
	var lockErr error
	err := control(f, func(fd uintptr) {
		flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
		if !wait {
			flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
		}
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
