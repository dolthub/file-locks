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

//go:build !linux && !darwin && !windows

package filelock

// This package deliberately fails to build where it cannot lock. A file lock
// that silently does not exclude is worse than no file lock at all, so there
// is no fallback implementation to fall into.
func init() {
	filelockSupportsOnlyLinuxDarwinAndWindows()
}
