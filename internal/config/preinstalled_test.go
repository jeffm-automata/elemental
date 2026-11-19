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

package config

import (
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suse/elemental/v3/internal/butane"
	"github.com/suse/elemental/v3/internal/image/kubernetes"
	"github.com/suse/elemental/v3/pkg/log"
	"github.com/suse/elemental/v3/pkg/sys"
	sysmock "github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

var _ = Describe("Preinstalled image configuration", func() {
	var output = Output{
		RootPath: "/_out",
	}

	var system *sys.System
	var fs vfs.FS
	var cleanup func()

	BeforeEach(func() {
		var err error

		fs, cleanup, err = sysmock.TestFS(map[string]any{
			"/config/kubernetes/images/rke2-images-core.linux-amd64.tar.zst": "core",
			"/config/kubernetes/images/custom.tar":                           "custom",
		})
		Expect(err).NotTo(HaveOccurred())

		system, err = sys.NewSystem(
			sys.WithLogger(log.New(log.WithDiscardAll())),
			sys.WithFS(fs),
		)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		cleanup()
	})

	It("adds the service growing the system partition and the script it runs", func() {
		var cfg butane.Config

		Expect(configurePreinstalled(&cfg)).To(Succeed())

		Expect(cfg.Systemd.Units).To(HaveLen(1))
		unit := cfg.Systemd.Units[0]
		Expect(unit.Name).To(Equal("elemental-grow-system.service"))
		Expect(*unit.Enabled).To(BeTrue())
		Expect(*unit.Contents).To(ContainSubstring(`ExecStart=/bin/sh "/var/lib/elemental/grow-system.sh"`))
		Expect(*unit.Contents).To(ContainSubstring("Before=k8s-config-installer.service"))

		Expect(cfg.Storage.Files).To(HaveLen(1))
		script := cfg.Storage.Files[0]
		Expect(script.Path).To(Equal("/var/lib/elemental/grow-system.sh"))
		Expect(*script.Contents.Inline).To(ContainSubstring("systemd-repart --dry-run=no"))
		Expect(*script.Contents.Inline).To(ContainSubstring("systemd-growfs /"))
	})

	It("places image archives where RKE2 imports them from", func() {
		k := &kubernetes.Kubernetes{LocalImages: []string{
			"/config/kubernetes/images/rke2-images-core.linux-amd64.tar.zst",
			"/config/kubernetes/images/custom.tar",
		}}

		Expect(NewManager(system, nil).preloadKubernetesImages(k, output)).To(Succeed())

		dir := filepath.Join(output.OverlaysDir(), "var/lib/rancher/rke2/agent/images")
		data, err := fs.ReadFile(filepath.Join(dir, "rke2-images-core.linux-amd64.tar.zst"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("core"))
		data, err = fs.ReadFile(filepath.Join(dir, "custom.tar"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("custom"))
	})

	It("does nothing without image archives", func() {
		Expect(NewManager(system, nil).preloadKubernetesImages(&kubernetes.Kubernetes{}, output)).To(Succeed())

		exists, _ := vfs.Exists(fs, filepath.Join(output.OverlaysDir(), "var"))
		Expect(exists).To(BeFalse())
	})

	It("fails on a missing image archive", func() {
		k := &kubernetes.Kubernetes{LocalImages: []string{"/config/kubernetes/images/missing.tar"}}

		err := NewManager(system, nil).preloadKubernetesImages(k, output)
		Expect(err).To(MatchError(ContainSubstring("copying image archive")))
	})
})
