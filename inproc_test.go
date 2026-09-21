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

import (
	"path/filepath"
	"testing"
)

func TestSlotTable(t *testing.T) {
	id := fileID{dev: 1, ino: 2}
	other := fileID{dev: 1, ino: 3}

	if !claimSlot(id) {
		t.Fatal("a free slot must be claimable")
	}
	defer freeSlot(id)

	if claimSlot(id) {
		freeSlot(id)
		t.Fatal("a taken slot was handed out twice")
	}
	if !claimSlot(other) {
		t.Fatal("a different file must have its own slot")
	}
	freeSlot(other)

	freeSlot(id)
	if !claimSlot(id) {
		t.Fatal("a freed slot must be claimable again")
	}
}

// Two Locks on one file agree on which slot they are competing for, whichever
// path each was opened through.
func TestLocksShareOneSlot(t *testing.T) {
	dir := t.TempDir()
	direct := newTest(t, filepath.Join(dir, "LOCK"))
	indirect := newTest(t, filepath.Join(dir, "x", "..", "LOCK"))

	if direct.id != indirect.id {
		t.Fatalf("Locks on one file have different ids: %+v and %+v", direct.id, indirect.id)
	}
	if direct.id == (fileID{}) {
		t.Error("the file has no identity at all")
	}
	if err := direct.TryLock(); err != nil {
		t.Fatal(err)
	}
	if err := indirect.TryLock(); err != ErrLocked {
		t.Errorf("TryLock = %v, want ErrLocked", err)
	}
}

// A held lock occupies exactly one slot, and gives it back.
func TestSlotIsHeldOnlyWhileTheLockIs(t *testing.T) {
	lck := newTest(t, lockPath(t))

	if slotHeld(lck.id) {
		t.Fatal("an unlocked Lock occupies a slot")
	}
	if err := lck.TryLock(); err != nil {
		t.Fatal(err)
	}
	if !slotHeld(lck.id) {
		t.Error("a held lock does not occupy its slot")
	}
	if err := lck.Unlock(); err != nil {
		t.Fatal(err)
	}
	if slotHeld(lck.id) {
		t.Error("the slot outlived the lock")
	}
}

// A failed attempt must not leave a slot behind, or the next one deadlocks
// against a holder that does not exist.
func TestSlotIsFreedByAFailedAttempt(t *testing.T) {
	path := lockPath(t)
	holder := held(t, path)
	waiter := newTest(t, path)

	if err := waiter.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}
	if err := holder.Unlock(); err != nil {
		t.Fatal(err)
	}
	if slotHeld(waiter.id) {
		t.Fatal("a refused attempt kept the slot")
	}
	if err := waiter.TryLock(); err != nil {
		t.Fatalf("TryLock after the holder left: %v", err)
	}
}

func slotHeld(id fileID) bool {
	inproc.mu.Lock()
	defer inproc.mu.Unlock()
	_, held := inproc.held[id]
	return held
}
