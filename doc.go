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

// Package filelock coordinates access to a resource among cooperating
// processes using an advisory lock on a file.
//
// A [Guard] is one participant's claim on one lock file. Open it once, then
// take and give back the claim as often as you like:
//
//	g, err := filelock.Open(filepath.Join(dir, "LOCK"))
//	if err != nil {
//		return err
//	}
//	defer g.Close()
//
//	if err := g.AcquireWithin(ctx, 100*time.Millisecond); err != nil {
//		if errors.Is(err, filelock.ErrWaitExpired) {
//			// someone else is working in dir; proceed read-only
//		}
//		return err
//	}
//	defer g.Release()
//
// # Guarantees
//
// While a Guard is acquired, no other Guard in this process and no other
// process using this package (or the underlying OS primitive) can acquire the
// same lock file. The claim is whole-file and exclusive; there is no shared
// (reader) mode.
//
// The operating system owns the claim, so it is released when the Guard is
// released or closed, when the process exits, and when the process is killed
// or crashes. This package never leaves a lock behind that a later run has to
// clean up, and it never deletes the lock file.
//
// # Platforms
//
// linux and darwin use flock(2); windows uses LockFileEx. Both are owned by
// the open file description or handle rather than by the process, so two
// Guards in one process exclude each other the same way two processes do.
// Other platforms do not build: an advisory lock that silently fails to
// exclude is worse than a compile error.
//
// # Limitations
//
// The lock is advisory. It binds only the participants that ask for it; code
// that writes the guarded files without taking the Guard is unaffected.
//
// Waiting is implemented by retrying with backoff rather than by blocking in
// the kernel, so a wait can always be abandoned by its context. There is no
// FIFO fairness: a Guard that starts waiting first is not guaranteed to
// acquire first.
//
// On network filesystems the guarantees are only as good as the mount. Linux
// implements flock(2) on NFS with POSIX record locks, and darwin may refuse it
// outright. Same-process exclusion is upheld regardless (see the in-process
// table in inproc.go), but cross-process exclusion over NFS or SMB should not
// be relied on. Keep lock files on local storage.
//
// A Guard holds its claim once. Acquiring one that is already acquired reports
// [ErrAlreadyHolding] instead of appearing to succeed; it is not a recursive
// mutex. A single *Guard is safe to use from multiple goroutines.
package filelock
