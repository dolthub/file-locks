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

// Package fslock takes exclusive advisory locks on files, so that cooperating
// processes can take turns.
//
//	lck, err := fslock.New(filepath.Join(dir, "LOCK"))
//	if err != nil {
//		return err
//	}
//	defer lck.Close()
//
//	if err := lck.LockWithTimeout(100 * time.Millisecond); err != nil {
//		if errors.Is(err, fslock.ErrTimeout) {
//			return openReadOnly(dir) // someone else is working here
//		}
//		return err
//	}
//	defer lck.Unlock()
//
// A [Lock] is a handle on one lock file. New opens it, Lock, TryLock,
// LockWithTimeout and LockWithContext take the lock, Unlock gives it back,
// and Close releases the handle. One Lock may be taken and given back any
// number of times.
//
// # Errors
//
// Every error this package reports is an [*Error], which answers
// [Error.IsTimeout] and [Error.IsTemporary]. The two that callers branch on
// are returned as the sentinel values [ErrLocked] and [ErrTimeout] - compared
// with == or with errors.Is, either way - so that a caller can tell "someone
// else has it" from "something went wrong".
//
// # Guarantees
//
// While a Lock is held, no other Lock in this process and no other process
// using this package can take the same file. Locks are exclusive; there is no
// shared mode.
//
// The operating system owns the lock, so it is released when the Lock is
// unlocked or closed, when the process exits, and when the process is killed
// or crashes. Nothing here has to clean up after a dead holder, and this
// package never deletes a lock file.
//
// # Platforms
//
// linux and darwin use flock(2); windows uses LockFileEx. Both are owned by
// the open file description or handle rather than by the process, so two
// Locks in one process exclude each other the same way two processes do.
// Other platforms do not build: an advisory lock that silently fails to
// exclude is worse than a compile error.
//
// Waiting differs by platform, by necessity. Windows can wait in the kernel
// and still be interrupted - LockFileEx takes an OVERLAPPED whose event a
// [WaitForMultipleObjects] can watch alongside a cancellation event - so it
// does. flock(2) offers no such handle: a thread blocked in it cannot be told
// to stop, so on unix a wait is a retry loop with backoff. The difference is
// invisible through the API, but it does mean unix waits wake up on a
// schedule rather than the instant the lock is free.
//
// # Limitations
//
// The lock is advisory. It binds the participants that ask for it and nothing
// else.
//
// On network filesystems the guarantees are only as good as the mount. Linux
// implements flock(2) on NFS with POSIX record locks, and darwin may refuse it
// outright. Same-process exclusion is upheld regardless (see inproc.go), but
// cross-process exclusion over NFS or SMB should not be relied on. Keep lock
// files on local storage.
//
// Waiters are not queued: a Lock that has waited longest has no claim to go
// first.
//
// A Lock holds its lock once. Taking one that is already taken reports an
// error rather than appearing to succeed, and a single *Lock is safe to use
// from several goroutines.
//
// One consequence is worth knowing: on windows a Close that overlaps a
// blocking Lock on the same Lock value waits for that call to finish, while
// on unix it interleaves with the retries. Bound the wait with
// [Lock.LockWithContext] or [Lock.LockWithTimeout] if that matters.
//
// [WaitForMultipleObjects]: https://learn.microsoft.com/en-us/windows/win32/api/synchapi/nf-synchapi-waitformultipleobjects
package fslock
