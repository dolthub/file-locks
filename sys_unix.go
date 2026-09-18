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

//go:build linux || darwin

package filelock

import (
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// On linux and darwin the claim is a flock(2) lock.
//
// flock locks belong to the open file description, not to the process, which
// is what lets two Guards in one process exclude each other: each has its own
// descriptor from its own open(2). (POSIX record locks - fcntl F_SETLK - would
// not do: they belong to the process, so a second Guard here would be handed a
// lock that is already held, and closing any descriptor for the file would
// drop locks taken through every other one.)
//
// The kernel drops the lock when the descriptor is closed and when the process
// dies, however it dies, so a crashed holder never leaves a claim behind.

// sysHandle is an open file descriptor.
type sysHandle int

func sysOpen(path string, mode os.FileMode) (sysHandle, error) {
	// O_CLOEXEC: a child process must not inherit the claim. Callers fork and
	// exec other tools while holding one.
	flags := unix.O_RDWR | unix.O_CREAT | unix.O_CLOEXEC
	for {
		fd, err := unix.Open(path, flags, uint32(mode.Perm()))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return -1, &fs.PathError{Op: "open", Path: path, Err: err}
		}
		return sysHandle(fd), nil
	}
}

func sysAcquire(h sysHandle, path string) (bool, error) {
	for {
		err := unix.Flock(int(h), unix.LOCK_EX|unix.LOCK_NB)
		switch err {
		case nil:
			return true, nil
		case unix.EINTR:
			continue
		case unix.EWOULDBLOCK:
			// Someone else holds it. An ordinary answer, not a failure.
			return false, nil
		default:
			return false, &fs.PathError{Op: "flock", Path: path, Err: err}
		}
	}
}

func sysRelease(h sysHandle, path string) error {
	for {
		err := unix.Flock(int(h), unix.LOCK_UN)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return &fs.PathError{Op: "funlock", Path: path, Err: err}
		}
		return nil
	}
}

func sysClose(h sysHandle) error {
	if err := unix.Close(int(h)); err != nil {
		return os.NewSyscallError("close", err)
	}
	return nil
}

func sysIdentify(h sysHandle) (fileID, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(h), &st); err != nil {
		return fileID{}, os.NewSyscallError("fstat", err)
	}
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

func sysOverwrite(h sysHandle, data []byte) error {
	if err := unix.Ftruncate(int(h), 0); err != nil {
		return os.NewSyscallError("ftruncate", err)
	}
	for off := 0; off < len(data); {
		n, err := unix.Pwrite(int(h), data[off:], int64(off))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return os.NewSyscallError("pwrite", err)
		}
		off += n
	}
	return nil
}
