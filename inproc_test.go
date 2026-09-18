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

// Two guards on one file agree on which slot they are competing for, whichever
// path each was opened through.
func TestGuardsShareOneSlot(t *testing.T) {
	dir := t.TempDir()
	direct := openTest(t, filepath.Join(dir, "LOCK"))
	indirect := openTest(t, filepath.Join(dir, "x", "..", "LOCK"))

	if direct.id != indirect.id {
		t.Fatalf("guards on one file have different ids: %+v and %+v", direct.id, indirect.id)
	}
	if direct.id == (fileID{}) {
		t.Error("the file has no identity at all")
	}
}

// A held claim occupies exactly one slot, and gives it back.
func TestSlotIsHeldOnlyWhileTheClaimIs(t *testing.T) {
	g := openTest(t, lockPath(t))

	if held := slotHeld(g.id); held {
		t.Fatal("an unacquired guard occupies a slot")
	}
	if !acquireNow(t, g) {
		t.Fatal("could not take the claim")
	}
	if !slotHeld(g.id) {
		t.Error("a held claim does not occupy its slot")
	}
	if err := g.Release(); err != nil {
		t.Fatal(err)
	}
	if slotHeld(g.id) {
		t.Error("the slot outlived the claim")
	}
}

func slotHeld(id fileID) bool {
	inproc.mu.Lock()
	defer inproc.mu.Unlock()
	_, held := inproc.held[id]
	return held
}
