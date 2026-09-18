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

package filelock

import "sync"

// fileID identifies the file a Guard is claiming, so that two Guards opened on
// the same file recognize each other even when they were opened through
// different paths. dev and ino come from the filesystem; path is the cleaned
// absolute path, consulted only when the filesystem could not answer.
type fileID struct {
	dev  uint64
	ino  uint64
	path string
}

// The in-process claim table.
//
// The OS primitives this package uses already exclude two claims within one
// process, so on a local filesystem this table is redundant. It exists for the
// case where they don't: Linux implements flock(2) on NFS with POSIX record
// locks, which are owned by the process, so two Guards in one process would
// both be told they hold an NFS-backed lock file. Taking a slot here first
// makes same-process exclusion a property of this package rather than a
// property of the mount.
//
// A slot is held for exactly as long as the Guard holds its claim, so the map
// only ever has an entry per actively held lock file.
var inproc struct {
	mu   sync.Mutex
	held map[fileID]struct{}
}

// claimSlot takes the in-process slot for id, reporting whether it was free.
func claimSlot(id fileID) bool {
	inproc.mu.Lock()
	defer inproc.mu.Unlock()
	if _, taken := inproc.held[id]; taken {
		return false
	}
	if inproc.held == nil {
		inproc.held = make(map[fileID]struct{})
	}
	inproc.held[id] = struct{}{}
	return true
}

// freeSlot gives back the in-process slot for id.
func freeSlot(id fileID) {
	inproc.mu.Lock()
	defer inproc.mu.Unlock()
	delete(inproc.held, id)
}
