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

package fslock

import "os"

// control runs fn on the file's descriptor or handle.
//
// Every system call this package makes on a lock file goes through here
// rather than through a descriptor we kept for ourselves. SyscallConn holds
// the file open for the duration of fn, so the descriptor cannot be closed
// and handed out to something else in the middle of a call - and the file
// came from os.OpenFile in the first place, which is what keeps it from
// leaking into a forked child. See openLockFile in lock.go.
func control(f *os.File, fn func(fd uintptr)) error {
	if f == nil {
		return os.ErrClosed
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	return rc.Control(fn)
}
