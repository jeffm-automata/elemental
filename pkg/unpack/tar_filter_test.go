/*
Copyright © 2025-2026 SUSE LLC
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package unpack

import (
	"archive/tar"
	"testing"
)

func TestOnlyFilesFilter(t *testing.T) {
	nextCalls := 0
	next := func(h *tar.Header) (bool, error) {
		nextCalls++
		return h.Name != "iso/excluded.iso", nil
	}
	filter := onlyFilesFilter([]string{"/iso/*.iso", "release_manifest.yaml"}, next)
	for _, c := range []struct {
		name string
		typ  byte
		want bool
	}{
		{"iso/os.iso", tar.TypeReg, true},
		{"./iso/os.iso", tar.TypeReg, true},
		{"/iso/os.iso", tar.TypeReg, true},
		{"iso/link.iso", tar.TypeSymlink, true},
		{"release_manifest.yaml", tar.TypeReg, true},
		// Directories are never extracted, not even a matching one: parents are created on demand.
		{"iso/", tar.TypeDir, false},
		{"iso/dir.iso", tar.TypeDir, false},
		{"usr/bin/[", tar.TypeReg, false},
		{"etc/shadow", tar.TypeReg, false},
		{"iso/sub/os.iso", tar.TypeReg, false},
		{"dev/null", tar.TypeChar, false},
		// The next filter (the caller's excludes) still has its say.
		{"iso/excluded.iso", tar.TypeReg, false},
	} {
		got, err := filter(&tar.Header{Name: c.name, Typeflag: c.typ})
		if err != nil || got != c.want {
			t.Errorf("%s (type %c): got %v, %v; want %v", c.name, c.typ, got, err, c.want)
		}
	}
	if nextCalls != 6 {
		t.Errorf("the next filter must only see matching files, saw %d entries", nextCalls)
	}
	if _, err := onlyFilesFilter([]string{"[bad"}, next)(&tar.Header{Name: "x", Typeflag: tar.TypeReg}); err == nil {
		t.Error("a malformed pattern must be an error")
	}
}
