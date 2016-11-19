// Copyright 2016 The rkt Authors
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

//+build linux

package fs

import (
	"strings"
	"syscall"
)

// String returns a human readable representation of mountFlags based on which bits are set.
// E.g. for a value of syscall.MS_RDONLY|syscall.MS_BIND it will print "MS_RDONLY|MS_BIND"
func (f mountFlags) String() string {
	var s []string

	appendFlag := func(ff uintptr, desc string) {
		if uintptr(f)&ff != 0 {
			s = append(s, desc)
		}
	}

	appendFlag(syscall.MS_DIRSYNC, "MS_DIRSYNC")
	appendFlag(syscall.MS_MANDLOCK, "MS_MANDLOCK")
	appendFlag(syscall.MS_NOATIME, "MS_NOATIME")
	appendFlag(syscall.MS_NODEV, "MS_NODEV")
	appendFlag(syscall.MS_NODIRATIME, "MS_NODIRATIME")
	appendFlag(syscall.MS_NOEXEC, "MS_NOEXEC")
	appendFlag(syscall.MS_NOSUID, "MS_NOSUID")
	appendFlag(syscall.MS_RDONLY, "MS_RDONLY")
	appendFlag(syscall.MS_REC, "MS_REC")
	appendFlag(syscall.MS_RELATIME, "MS_RELATIME")
	appendFlag(syscall.MS_SILENT, "MS_SILENT")
	appendFlag(syscall.MS_STRICTATIME, "MS_STRICTATIME")
	appendFlag(syscall.MS_SYNCHRONOUS, "MS_SYNCHRONOUS")
	appendFlag(syscall.MS_REMOUNT, "MS_REMOUNT")
	appendFlag(syscall.MS_BIND, "MS_BIND")
	appendFlag(syscall.MS_SHARED, "MS_SHARED")
	appendFlag(syscall.MS_PRIVATE, "MS_PRIVATE")
	appendFlag(syscall.MS_SLAVE, "MS_SLAVE")
	appendFlag(syscall.MS_UNBINDABLE, "MS_UNBINDABLE")
	appendFlag(syscall.MS_MOVE, "MS_MOVE")

	return strings.Join(s, "|")
}
