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

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/suse/elemental/v3/pkg/deployment"
	"github.com/suse/elemental/v3/pkg/sys/platform"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	// preinstallTimeout bounds the build-time installation. With hardware virtualization the
	// installation takes a few minutes; without it (emulation) it is several times slower.
	preinstallTimeout = 45 * time.Minute

	preinstallMemoryMiB = "4096"
	preinstallCPUs      = "4"
	preinstallSerialLog = "preinstall-console.log"
)

// firmware is a UEFI firmware image pair as shipped by a distribution
type firmware struct {
	code string
	vars string
}

type qemuPlatform struct {
	binary   string
	machine  string
	firmware []firmware
}

func qemuFor(p *platform.Platform) (*qemuPlatform, error) {
	switch p.Arch {
	case platform.ArchAmd64, platform.Archx86:
		return &qemuPlatform{
			binary:  "qemu-system-x86_64",
			machine: "q35,accel=kvm:tcg",
			firmware: []firmware{
				{"/usr/share/qemu/ovmf-x86_64-4m-code.bin", "/usr/share/qemu/ovmf-x86_64-4m-vars.bin"},
				{"/usr/share/qemu/ovmf-x86_64-code.bin", "/usr/share/qemu/ovmf-x86_64-vars.bin"},
				{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
				{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
				{"/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
			},
		}, nil
	case platform.ArchArm64, platform.ArchAarch64:
		// Not tested.
		return &qemuPlatform{
			binary:  "qemu-system-aarch64",
			machine: "virt,gic-version=max,accel=kvm:tcg",
			firmware: []firmware{
				{"/usr/share/qemu/aavmf-aarch64-code.bin", "/usr/share/qemu/aavmf-aarch64-vars.bin"},
				{"/usr/share/AAVMF/AAVMF_CODE.fd", "/usr/share/AAVMF/AAVMF_VARS.fd"},
				{"/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw", "/usr/share/edk2/aarch64/vars-template-pflash.raw"},
			},
		}, nil
	default:
		return nil, fmt.Errorf("no virtual machine definition for architecture '%s': %w", p.Arch, errors.ErrUnsupported)
	}
}

// preinstallDisk turns the installer disk image rawImg into an installed disk image. It boots the
// image once in a virtual machine without network: the image installs itself to its own disk, exactly
// as it does on the first boot of a real machine, and asks for a reboot, at which point the virtual
// machine is stopped. The installed system is never booted, so everything a system creates on its own
// first boot (machine ID, SSH host keys, first-boot configuration) is still created on each machine.
func (i Media) preinstallDisk(tempDir, rawImg string, d *deployment.Deployment) error {
	q, err := qemuFor(i.s.Platform())
	if err != nil {
		return err
	}

	var fw *firmware
	for _, f := range q.firmware {
		code, _ := vfs.Exists(i.s.FS(), f.code)
		vars, _ := vfs.Exists(i.s.FS(), f.vars)
		if code && vars {
			fw = &f
			break
		}
	}
	if fw == nil {
		return fmt.Errorf("no UEFI firmware for %s found, a preinstalled image requires qemu and its UEFI firmware", q.binary)
	}

	// The firmware variable store is written by the virtual machine, work on a copy.
	vars := filepath.Join(tempDir, "preinstall-efivars.bin")
	if err = vfs.CopyFile(i.s.FS(), fw.vars, vars); err != nil {
		return fmt.Errorf("copying UEFI variable store '%s': %w", fw.vars, err)
	}
	defer func() { _ = i.s.FS().Remove(vars) }()

	serialLog := filepath.Join(filepath.Dir(i.outputFile), preinstallSerialLog)

	args := []string{
		"-machine", q.machine,
		"-cpu", "max",
		"-m", preinstallMemoryMiB,
		"-smp", preinstallCPUs,
		"-display", "none",
		"-monitor", "none",
		"-serial", "file:" + serialLog,
		"-nic", "none",
		// The installer reboots when it is done: stop instead, the installed system must not boot here.
		"-no-reboot",
		"-device", "virtio-rng-pci",
		"-drive", fmt.Sprintf("if=pflash,format=raw,readonly=on,file=%s", fw.code),
		"-drive", fmt.Sprintf("if=pflash,format=raw,file=%s", vars),
		"-drive", fmt.Sprintf("if=virtio,format=raw,cache=unsafe,discard=unmap,file=%s", rawImg),
	}

	i.s.Logger().Info("Installing the system to '%s' in a virtual machine, console output in '%s'", rawImg, serialLog)

	ctx, cancel := context.WithTimeout(i.ctx, preinstallTimeout)
	defer cancel()

	start := time.Now()
	out, err := i.s.Runner().RunContext(ctx, q.binary, args...)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("installation in a virtual machine did not finish within %s, see '%s': %w", preinstallTimeout, serialLog, ctx.Err())
		}
		return fmt.Errorf("running %s: %w: %s", q.binary, err, string(out))
	}

	// The virtual machine also stops when the installer gives up. The installation succeeded
	// only if the disk now has its system partition.
	label := d.GetSystemLabel()
	table, err := i.s.Runner().Run("sfdisk", "--dump", rawImg)
	if err != nil {
		return fmt.Errorf("reading partition table of '%s': %w", rawImg, err)
	}
	if !strings.Contains(string(table), fmt.Sprintf("name=\"%s\"", label)) {
		return fmt.Errorf("installation in a virtual machine left no '%s' partition, see '%s'", label, serialLog)
	}

	i.s.Logger().Info("System installed to the image in %s", time.Since(start).Round(time.Second))
	return nil
}
