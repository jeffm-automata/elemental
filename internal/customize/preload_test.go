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

package customize_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suse/elemental/v3/internal/customize"
	"github.com/suse/elemental/v3/pkg/deployment"
	"github.com/suse/elemental/v3/pkg/installer"
	sysmock "github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

var _ = Describe("Installer script of an image with preloaded container images", func() {
	var fs vfs.FS
	var cleanup func()

	script := func(mediaType installer.MediaType, volume string) string {
		path, err := customize.WriteAutoInstaller(fs, "/_out", mediaType, "SYSTEM", volume)
		Expect(err).NotTo(HaveOccurred())
		data, err := fs.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		return string(data)
	}

	BeforeEach(func() {
		var err error
		fs, cleanup, err = sysmock.TestFS(map[string]any{"/_out": map[string]any{}})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		cleanup()
	})

	It("is unchanged without preloaded images", func() {
		for _, mediaType := range []installer.MediaType{installer.Disk, installer.ISO} {
			Expect(script(mediaType, "")).NotTo(ContainSubstring("preload"))
		}
	})

	It("fills the image store of the installed system after the installation", func() {
		data := script(installer.Disk, "@/var")

		Expect(data).To(ContainSubstring("ExecStart=/usr/bin/elemental3ctl --debug reset\nExecStartPost=-/bin/bash /etc/elemental-preload-images.sh\n"))
		Expect(data).To(ContainSubstring(`runtime="/dev/disk/by-id/virtio-elemental-runtime"`))
		Expect(data).To(ContainSubstring(`images="/dev/disk/by-id/virtio-elemental-images"`))
		// Without the two build-time disks the script ends before it touches anything.
		Expect(data).To(ContainSubstring(`[[ -b "${runtime}" && -b "${images}" ]] || exit 0` + "\n\nwork="))
		Expect(data).To(ContainSubstring(`mount -o "subvol=@/var" "/dev/disk/by-label/SYSTEM" "${work}/var"`))
		Expect(data).To(ContainSubstring(`ctr images import "${images}"`))
		Expect(data).To(ContainSubstring(`say "done, ${count} images`))
		Expect(data).To(ContainSubstring(`echo "elemental-preload: $*" 2> /dev/null > "${port}"`))
	})

	It("finds the volume of the installed system that holds the image store", func() {
		d := deployment.DefaultDeployment()
		Expect(customize.VarSubvolume(d)).To(Equal("@/var"))
	})

	It("fails when /var is not a plain read-write volume", func() {
		d := deployment.DefaultDeployment()
		system := d.GetSystemPartition()
		for i := range system.RWVolumes {
			if system.RWVolumes[i].Path == "/var" {
				system.RWVolumes[i].Snapshotted = true
			}
		}

		_, err := customize.VarSubvolume(d)
		Expect(err).To(MatchError(ContainSubstring("requires '/var' to be a read-write volume")))
	})
})
