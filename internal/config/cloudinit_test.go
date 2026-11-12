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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suse/elemental/v3/internal/butane"
	"github.com/suse/elemental/v3/internal/image"
	"github.com/suse/elemental/v3/internal/image/install"
	"github.com/suse/elemental/v3/pkg/log"
	"github.com/suse/elemental/v3/pkg/sys"
	sysmock "github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

var _ = Describe("Cloud-init configuration", func() {
	var output = Output{
		RootPath: "/_out",
	}

	var system *sys.System
	var fs vfs.FS
	var cleanup func()
	var buffer *bytes.Buffer
	var binaryErr error

	newManager := func(opts ...Opts) *Manager {
		opts = append([]Opts{WithCloudInitBinary(func() (string, error) {
			return "/usr/bin/elemental3ctl", binaryErr
		})}, opts...)
		return NewManager(system, nil, opts...)
	}

	BeforeEach(func() {
		var err error

		binaryErr = nil
		buffer = &bytes.Buffer{}
		fs, cleanup, err = sysmock.TestFS(map[string]any{
			"/usr/bin/elemental3ctl": "binary",
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(vfs.MkdirAll(fs, output.RootPath, vfs.DirPerm)).To(Succeed())

		system, err = sys.NewSystem(
			sys.WithLogger(log.New(log.WithBuffer(buffer))),
			sys.WithFS(fs),
		)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		cleanup()
	})

	It("includes the services and the binary they run", func() {
		var cfg butane.Config

		Expect(newManager().configureCloudInit(output, &cfg)).To(Succeed())

		target := filepath.Join(output.OverlaysDir(), "usr/local/libexec/elemental/elemental3ctl")
		data, err := fs.ReadFile(target)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("binary"))
		info, err := fs.Stat(target)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o755)))

		Expect(cfg.Systemd.Units).To(HaveLen(3))

		early := cfg.Systemd.Units[0]
		Expect(early.Name).To(Equal("elemental-cloud-init-early.service"))
		Expect(*early.Enabled).To(BeTrue())
		Expect(*early.Contents).To(ContainSubstring(
			"ExecStart=/usr/local/libexec/elemental/elemental3ctl cloud-init --fetch --stage initramfs --stage fs"))
		Expect(*early.Contents).To(ContainSubstring(
			"Before=sysinit.target network-pre.target k8s-config-installer.service shutdown.target"))

		boot := cfg.Systemd.Units[1]
		Expect(boot.Name).To(Equal("elemental-cloud-init-boot.service"))
		Expect(*boot.Contents).To(ContainSubstring("After=elemental-cloud-init-early.service"))
		Expect(*boot.Contents).To(ContainSubstring("cloud-init --stage boot"))

		network := cfg.Systemd.Units[2]
		Expect(network.Name).To(Equal("elemental-cloud-init-network.service"))
		Expect(*network.Contents).To(ContainSubstring(
			"After=network-online.target elemental-cloud-init-boot.service k8s-config-installer.service"))
		Expect(*network.Contents).To(ContainSubstring("cloud-init --stage network --finalize"))
	})

	It("fails if the binary is not found", func() {
		var cfg butane.Config

		binaryErr = fmt.Errorf("not found in PATH")
		err := newManager().configureCloudInit(output, &cfg)
		Expect(err).To(MatchError("locating elemental3ctl executable: not found in PATH"))
		Expect(cfg.Systemd.Units).To(BeEmpty())
	})

	It("fails if the image platform differs from the host platform", func() {
		var cfg butane.Config

		err := newManager(WithPlatform("linux/s390x")).configureCloudInit(output, &cfg)
		Expect(err).To(MatchError(ContainSubstring("no elemental3ctl binary available for platform linux/s390x")))
	})

	It("is only included in the Ignition configuration if enabled", func() {
		conf := &image.Configuration{}

		Expect(newManager().configureSystem(context.Background(), conf, output, nil, nil)).To(Succeed())
		Expect(vfs.Exists(fs, output.InitrdExtensionFile())).To(BeFalse())

		conf.Installation = install.Installation{CloudInit: install.CloudInit{Enabled: true}}
		Expect(newManager().configureSystem(context.Background(), conf, output, nil, nil)).To(Succeed())

		ignition, err := fs.ReadFile(output.InitrdExtensionFile())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(ignition)).To(ContainSubstring("elemental-cloud-init-early.service"))
		Expect(string(ignition)).To(ContainSubstring("elemental-cloud-init-network.service"))
	})
})
