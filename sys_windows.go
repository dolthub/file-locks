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

package filelock

import (
	"errors"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// On windows the claim is a LockFileEx byte-range lock.
//
// Byte-range locks belong to the handle they were taken through, which gives
// the same shape as flock(2) on unix: two Guards in one process each have
// their own handle and exclude each other. Windows drops the lock when the
// handle is closed and when the process dies, so a crashed holder leaves
// nothing behind.

// sysHandle is an open file handle.
type sysHandle windows.Handle

// The locked range is a single byte far past any content the file will ever
// have, so that a holder stamp written at offset 0 never overlaps it.
const (
	lockOffsetHigh = 0x80000000
	lockBytes      = 1
)

func lockRegion() *windows.Overlapped {
	return &windows.Overlapped{OffsetHigh: lockOffsetHigh}
}

func sysOpen(path string, mode os.FileMode) (sysHandle, error) {
	_ = mode // windows takes its permissions from the directory's ACL

	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return sysHandle(windows.InvalidHandle), &fs.PathError{Op: "open", Path: path, Err: err}
	}
	// FILE_SHARE_DELETE so that removing the directory a lock file lives in
	// is not blocked by this handle; no inheritance, so a child process does
	// not carry the claim.
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
		return sysHandle(windows.InvalidHandle), &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return sysHandle(h), nil
}

func sysAcquire(h sysHandle, path string) (bool, error) {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	err := windows.LockFileEx(windows.Handle(h), flags, 0, lockBytes, 0, lockRegion())
	if err == nil {
		return true, nil
	}
	// ERROR_LOCK_VIOLATION is the refusal LOCKFILE_FAIL_IMMEDIATELY produces;
	// ERROR_IO_PENDING has been seen in its place on some versions.
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, &fs.PathError{Op: "lockfileex", Path: path, Err: err}
}

func sysRelease(h sysHandle, path string) error {
	if err := windows.UnlockFileEx(windows.Handle(h), 0, lockBytes, 0, lockRegion()); err != nil {
		return &fs.PathError{Op: "unlockfileex", Path: path, Err: err}
	}
	return nil
}

func sysClose(h sysHandle) error {
	if err := windows.CloseHandle(windows.Handle(h)); err != nil {
		return os.NewSyscallError("closehandle", err)
	}
	return nil
}

func sysIdentify(h sysHandle) (fileID, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(h), &info); err != nil {
		return fileID{}, os.NewSyscallError("getfileinformationbyhandle", err)
	}
	return fileID{
		dev: uint64(info.VolumeSerialNumber),
		ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

func sysOverwrite(h sysHandle, data []byte) error {
	if _, err := windows.Seek(windows.Handle(h), 0, io.SeekStart); err != nil {
		return os.NewSyscallError("seek", err)
	}
	for off := 0; off < len(data); {
		n, err := windows.Write(windows.Handle(h), data[off:])
		if err != nil {
			return os.NewSyscallError("write", err)
		}
		off += n
	}
	if err := windows.SetEndOfFile(windows.Handle(h)); err != nil {
		return os.NewSyscallError("setendoffile", err)
	}
	return nil
}
