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

package installer

import "testing"

const sfdiskDump = `label: gpt
label-id: 6D1C4C4B-0B0E-4E0B-9C0A-3F0C5B8D1E2F
device: /tmp/disk.raw
unit: sectors
first-lba: 2048
last-lba: 41943006
sector-size: 512

/tmp/disk.raw1 : start=        2048, size=     2097152, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, uuid=0F1E2D3C-0000-4000-8000-000000000001, name="EFI"
/tmp/disk.raw2 : start=     2099200, size=     3145728, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=0F1E2D3C-0000-4000-8000-000000000002, name="RECOVERY"
/tmp/disk.raw3 : start=     5244928, size=    36698079, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=0F1E2D3C-0000-4000-8000-000000000003, name="SYSTEM"
`

func TestESPOffset(t *testing.T) {
	off, err := espOffset(sfdiskDump)
	if err != nil || off != 2048*512 {
		t.Fatalf("offset %d, error %v; want %d", off, err, 2048*512)
	}
	// 4096 byte sectors, lower case type as some sfdisk versions print it
	dump := "sector-size: 4096\n\n/dev/x1 : start=256, size=1000, type=c12a7328-f81f-11d2-ba4b-00a0c93ec93b\n"
	if off, err = espOffset(dump); err != nil || off != 256*4096 {
		t.Fatalf("offset %d, error %v; want %d", off, err, 256*4096)
	}
	if _, err = espOffset("label: gpt\n\n/dev/x1 : start=2048, size=10, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4\n"); err == nil {
		t.Fatal("a table without an EFI system partition must be an error")
	}
}
