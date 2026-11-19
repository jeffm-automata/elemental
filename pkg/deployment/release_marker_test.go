/*
Copyright © 2026 SUSE LLC
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

package deployment_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suse/elemental/v3/pkg/deployment"
	"github.com/suse/elemental/v3/pkg/log"
	"github.com/suse/elemental/v3/pkg/sys"
	sysmock "github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

var _ = Describe("ReleaseMarker", Label("deployment"), func() {
	const (
		root       = "/some/root"
		markerPath = root + "/etc/elemental/release.yaml"
		manifest   = "oci://registry.example.com/release-manifest:1.2.3"
		osImage    = "registry.example.com/base-os:16.1-76.43"
		isoImage   = "registry.example.com/base-os-iso:16.1-76.81"
	)

	var s *sys.System
	var tfs vfs.FS
	var cleanup func()
	var err error

	BeforeEach(func() {
		tfs, cleanup, err = sysmock.TestFS(nil)
		Expect(err).NotTo(HaveOccurred())
		s, err = sys.NewSystem(
			sys.WithFS(tfs),
			sys.WithLogger(log.New(log.WithDiscardAll())),
		)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		cleanup()
	})

	It("is stored next to the deployment file", func() {
		Expect(deployment.ReleaseMarkerFile).To(Equal("/etc/elemental/release.yaml"))
	})

	It("writes one plain 'key: value' line per reference", func() {
		marker := deployment.ReleaseMarker{ReleaseManifest: manifest, OSImage: osImage, InstallerImage: isoImage}
		Expect(marker.Write(s, root)).To(Succeed())

		data, err := tfs.ReadFile(markerPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal(`# self-generated content, do not edit

releaseManifest: oci://registry.example.com/release-manifest:1.2.3
osImage: registry.example.com/base-os:16.1-76.43
installerImage: registry.example.com/base-os-iso:16.1-76.81
`))
	})

	It("keeps image references with a digest on a single plain line", func() {
		digestRef := "registry.example.com/base-os@sha256:2d195da213f9ae85ff9444f224aa487e9dbd3cd8891599ae022eeb5f04a17fb2"
		Expect(deployment.ReleaseMarker{ReleaseManifest: manifest, OSImage: digestRef}.Write(s, root)).To(Succeed())

		data, err := tfs.ReadFile(markerPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("\nosImage: " + digestRef + "\n"))
		Expect(string(data)).NotTo(ContainSubstring("installerImage"))
	})

	It("reads back what it wrote", func() {
		marker := deployment.ReleaseMarker{ReleaseManifest: manifest, OSImage: osImage, InstallerImage: isoImage}
		Expect(marker.Write(s, root)).To(Succeed())

		read, err := deployment.ParseReleaseMarker(s, root)
		Expect(err).NotTo(HaveOccurred())
		Expect(read).NotTo(BeNil())
		Expect(*read).To(Equal(marker))
	})

	It("overwrites a previous marker", func() {
		Expect(deployment.ReleaseMarker{ReleaseManifest: manifest, OSImage: "registry.example.com/old:1"}.Write(s, root)).To(Succeed())
		Expect(deployment.ReleaseMarker{ReleaseManifest: manifest, OSImage: osImage}.Write(s, root)).To(Succeed())

		read, err := deployment.ParseReleaseMarker(s, root)
		Expect(err).NotTo(HaveOccurred())
		Expect(read.OSImage).To(Equal(osImage))
	})

	It("ignores fields it does not know, unlike the deployment file", func() {
		Expect(vfs.MkdirAll(tfs, root+"/etc/elemental", vfs.DirPerm)).To(Succeed())
		Expect(tfs.WriteFile(markerPath, []byte("osImage: "+osImage+"\nfutureField: value\n"), 0444)).To(Succeed())

		read, err := deployment.ParseReleaseMarker(s, root)
		Expect(err).NotTo(HaveOccurred())
		Expect(read.OSImage).To(Equal(osImage))
	})

	It("returns nothing if there is no marker", func() {
		read, err := deployment.ParseReleaseMarker(s, root)
		Expect(err).NotTo(HaveOccurred())
		Expect(read).To(BeNil())
	})

	It("fails to read a marker that is not valid yaml", func() {
		Expect(vfs.MkdirAll(tfs, root+"/etc/elemental", vfs.DirPerm)).To(Succeed())
		Expect(tfs.WriteFile(markerPath, []byte("osImage: [unterminated"), 0444)).To(Succeed())

		_, err := deployment.ParseReleaseMarker(s, root)
		Expect(err).To(HaveOccurred())
	})

	It("removes the marker and leaves the deployment file alone", func() {
		d := deployment.DefaultDeployment()
		d.SourceOS = deployment.NewRawSrc("/run/initramfs/live/LiveOS/squashfs.img")
		Expect(d.WriteDeploymentFile(s, root)).To(Succeed())
		Expect(deployment.ReleaseMarker{ReleaseManifest: manifest, OSImage: osImage}.Write(s, root)).To(Succeed())

		Expect(deployment.RemoveReleaseMarker(s, root)).To(Succeed())

		Expect(vfs.Exists(tfs, markerPath)).To(BeFalse())
		rD, err := deployment.Parse(s, root)
		Expect(err).NotTo(HaveOccurred())
		Expect(rD).NotTo(BeNil())
	})

	It("does not fail removing a marker that does not exist", func() {
		Expect(deployment.RemoveReleaseMarker(s, root)).To(Succeed())
	})
})
