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

package installer_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suse/elemental/v3/pkg/installer"
	sysmock "github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

var _ = Describe("Result of the image preload on the console of the build-time installation", func() {
	const console = "/build/preinstall-console.log"

	var fs vfs.FS
	var cleanup func()

	write := func(content string) {
		Expect(fs.WriteFile(console, []byte(content), vfs.FilePerm)).To(Succeed())
	}

	BeforeEach(func() {
		var err error
		fs, cleanup, err = sysmock.TestFS(map[string]any{"/build": map[string]any{}})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		cleanup()
	})

	It("returns what the preload reports when it is done", func() {
		write("[   61.2][ T900] systemd[1]: Starting Elemental Autoinstall...\r\n" +
			"[  183.4][ T1411] elemental-preload: done, 33 images, 131 layers, 6012 MiB in the image store of the installed system\r\n" +
			"[  184.0][    T1] reboot: Restarting system\r\n")

		result, err := installer.PreloadResult(fs, console)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal("33 images, 131 layers, 6012 MiB in the image store of the installed system"))
	})

	It("fails with the lines of a preload that failed", func() {
		write("[  100.1][ T1411] elemental-preload: cannot import the images\n" +
			"[  100.2][ T1412] elemental-preload: ctr: content digest sha256:aa: not found\n" +
			"[  101.0][    T1] reboot: Restarting system\n")

		_, err := installer.PreloadResult(fs, console)
		Expect(err).To(MatchError(ContainSubstring("preloading the container images failed")))
		Expect(err).To(MatchError(ContainSubstring("cannot import the images; ctr: content digest sha256:aa: not found")))
	})

	It("fails when the installation never ran the preload", func() {
		write("[  101.0][    T1] reboot: Restarting system\n")

		_, err := installer.PreloadResult(fs, console)
		Expect(err).To(MatchError(ContainSubstring("the installation did not run the preload")))
	})

	It("fails without a console log", func() {
		_, err := installer.PreloadResult(fs, console)
		Expect(err).To(MatchError(ContainSubstring("reading console output")))
	})
})

var _ = Describe("Disks of the build-time installation", func() {
	It("has the image as its only disk without an image preload, as before", func() {
		Expect(installer.PreinstallDisks("/build/disk.raw", nil)).To(Equal([]string{
			"-drive", "if=virtio,format=raw,cache=unsafe,discard=unmap,file=/build/disk.raw",
		}))
	})

	It("keeps the image the first disk when the archives of an image preload are attached", func() {
		// The installation records its disk as /dev/vda and an upgrade of the installed system
		// uses that name: the image must not become /dev/vdc because two disks were added.
		args := installer.PreinstallDisks("/build/disk.raw", &installer.ImagePreload{
			RuntimeArchive: "/store/runtime.tar",
			ImagesArchive:  "/store/images.tar",
		})

		Expect(args).To(Equal([]string{
			"-drive", "if=none,id=system,format=raw,cache=unsafe,discard=unmap,file=/build/disk.raw",
			"-device", "virtio-blk-pci,drive=system,bootindex=0",
			"-drive", "if=none,id=elemental-runtime,format=raw,readonly=on,file=/store/runtime.tar",
			"-device", "virtio-blk-pci,drive=elemental-runtime,serial=elemental-runtime",
			"-drive", "if=none,id=elemental-images,format=raw,readonly=on,file=/store/images.tar",
			"-device", "virtio-blk-pci,drive=elemental-images,serial=elemental-images",
		}))
		Expect(args).NotTo(ContainElement(ContainSubstring("if=virtio")))
	})
})
