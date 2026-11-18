// Copyright 2026 Dolthub, Inc. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the root of this repository.

//go:build windows

package fslock

import (
	"errors"
	"os"
	"unsafe"

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

// sysOpen opens a synchronous, non-inheritable lock handle with deletion shared.
// NtCreateFile distinguishes STATUS_DELETE_PENDING from a genuine permission
// failure, although CreateFile reports both as access denied.
func sysOpen(path string) (*os.File, error) {
	return openWindowsFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_OPEN_IF)
}

// sysStat reads identity through an existing metadata handle so a deleted file
// reports a missing name rather than an ambiguous permission error.
func sysStat(path string) (os.FileInfo, error) {
	f, err := openWindowsFile(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	return info, errors.Join(err, f.Close())
}

var rtlFreeUnicodeString = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlFreeUnicodeString")

// openWindowsFile preserves native deletion status while keeping normal Go
// filesystem error matching for other failures. Rtl handles DOS, relative, UNC,
// and extended paths and allocates the native name that RtlFreeUnicodeString frees.
func openWindowsFile(path string, access, disposition uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var name windows.NTUnicodeString
	if err := windows.RtlDosPathNameToNtPathName(p, &name, nil, nil); err != nil {
		return nil, windowsFileError(err)
	}
	defer rtlFreeUnicodeString.Call(uintptr(unsafe.Pointer(&name)))
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:     uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		ObjectName: &name,
		Attributes: windows.OBJ_CASE_INSENSITIVE,
	}
	var handle windows.Handle
	var result windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access|windows.SYNCHRONIZE, &attributes, &result,
		nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition, windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_NON_DIRECTORY_FILE,
		0, 0)
	if err != nil {
		return nil, windowsFileError(err)
	}
	return os.NewFile(uintptr(handle), path), nil
}

// windowsFileError treats a deleted name as missing while its old handles close.
// Native status distinguishes this state from genuine permission failures.
func windowsFileError(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		if status == windows.STATUS_DELETE_PENDING {
			return os.ErrNotExist
		}
		return status.Errno()
	}
	return err
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
