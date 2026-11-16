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

package cloudinit_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/twpayne/go-vfs/v4/vfst"

	"github.com/suse/elemental/v3/internal/cloudinit"
	"github.com/suse/elemental/v3/pkg/log"
	"github.com/suse/elemental/v3/pkg/sys"
	"github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	mnt        = cloudinit.RunDir + "/mnt"
	configFile = cloudinit.RunDir + "/config/00_userdata.yaml"
	pendingID  = cloudinit.RunDir + "/instance-id"
	appliedID  = cloudinit.StateDir + "/instance-id"
)

type fakeRunner struct {
	stages  []string
	sources [][]string
	err     error
}

func (f *fakeRunner) Run(stage string, sources ...string) error {
	f.stages = append(f.stages, stage)
	f.sources = append(f.sources, sources)
	return f.err
}

var _ = Describe("Cloud-init", func() {
	var s *sys.System
	var tfs vfs.FS
	var cleanup func()
	var runner *mock.Runner
	var mounter *mock.Mounter
	var logBuffer *bytes.Buffer
	var cfg *cloudinit.Config

	// setup creates a system whose filesystem has the given files. The mount of a datasource
	// volume is simulated by having its files already at the mount point.
	setup := func(files map[string]any) {
		var err error

		tfs, cleanup, err = mock.TestFS(files)
		Expect(err).NotTo(HaveOccurred())

		runner = mock.NewRunner()
		mounter = mock.NewMounter()
		logBuffer = bytes.NewBuffer(nil)

		s, err = sys.NewSystem(sys.WithFS(tfs),
			sys.WithRunner(runner),
			sys.WithMounter(mounter),
			sys.WithLogger(log.New(log.WithBuffer(logBuffer))))
		Expect(err).NotTo(HaveOccurred())

		cfg = cloudinit.NewConfig()
	}

	AfterEach(func() {
		cleanup()
	})

	Describe("Datasources", func() {
		It("returns ErrNoDatasource if there is no datasource volume", func() {
			setup(map[string]any{"/dev/disk/by-label/SYSTEM": ""})
			_, err := cloudinit.FetchUserData(s, mnt, 0)
			Expect(err).To(MatchError(cloudinit.ErrNoDatasource))
		})

		It("gives up once the timeout expires", func() {
			setup(map[string]any{})
			start := time.Now()
			_, err := cloudinit.FetchUserData(s, mnt, time.Second)
			Expect(err).To(MatchError(cloudinit.ErrNoDatasource))
			Expect(time.Since(start)).To(BeNumerically(">=", time.Second))
		})

		It("reads user-data and metadata from a NoCloud volume", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "#cloud-config\nhostname: foo\n",
				mnt + "/meta-data":          "instance-id: vm1.agents\nlocal-hostname: vm1\n",
			})
			ud, err := cloudinit.FetchUserData(s, mnt, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(ud.Source).To(Equal("NoCloud (/dev/disk/by-label/cidata)"))
			Expect(ud.InstanceID).To(Equal("vm1.agents"))
			Expect(ud.Hostname).To(Equal("vm1"))
			Expect(string(ud.Data)).To(Equal("#cloud-config\nhostname: foo\n"))

			By("mounting the volume read-only and unmounting it afterwards")
			Expect(mounter.IsMountPoint(mnt)).To(BeFalse())
			Expect(logBuffer.String()).To(ContainSubstring("Found NoCloud datasource at device /dev/disk/by-label/cidata"))
		})

		It("reads user-data and metadata from a ConfigDrive volume", func() {
			setup(map[string]any{
				"/dev/disk/by-label/config-2":            "",
				mnt + "/openstack/latest/user_data":      "#!/bin/sh\necho\n",
				mnt + "/openstack/latest/meta_data.json": `{"uuid": "abc-123", "hostname": "vm2", "name": "x"}`,
			})
			ud, err := cloudinit.FetchUserData(s, mnt, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(ud.Source).To(Equal("ConfigDrive (/dev/disk/by-label/config-2)"))
			Expect(ud.InstanceID).To(Equal("abc-123"))
			Expect(ud.Hostname).To(Equal("vm2"))
			Expect(string(ud.Data)).To(Equal("#!/bin/sh\necho\n"))
		})

		It("accepts a volume without user-data or metadata", func() {
			setup(map[string]any{"/dev/disk/by-label/CIDATA": "", mnt: &vfst.Dir{Perm: 0o755}})
			ud, err := cloudinit.FetchUserData(s, mnt, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(ud.Data).To(BeEmpty())
			Expect(ud.InstanceID).To(BeEmpty())
			Expect(logBuffer.String()).To(ContainSubstring("NoCloud datasource has no 'user-data' file"))
			Expect(logBuffer.String()).To(ContainSubstring("NoCloud datasource has no 'meta-data' file"))
		})

		It("fails with a detailed error if the volume cannot be mounted", func() {
			setup(map[string]any{"/dev/disk/by-label/cidata": ""})
			mounter.ErrorOnMount = true
			_, err := cloudinit.FetchUserData(s, mnt, 0)
			Expect(err).To(MatchError(ContainSubstring(
				"mounting NoCloud datasource device '/dev/disk/by-label/cidata' at '/run/elemental/cloud-init/mnt'")))
		})

		It("fails with a detailed error on invalid metadata", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/meta-data":          "instance-id: [",
			})
			_, err := cloudinit.FetchUserData(s, mnt, 0)
			Expect(err).To(MatchError(ContainSubstring("parsing metadata file 'meta-data' of NoCloud datasource")))
			Expect(mounter.IsMountPoint(mnt)).To(BeFalse())
		})
	})

	Describe("Fetch", func() {
		It("does nothing without a datasource", func() {
			setup(map[string]any{})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())
			Expect(logBuffer.String()).To(ContainSubstring("No cloud-init datasource found"))
			Expect(vfs.Exists(tfs, configFile)).To(BeFalse())
		})

		It("stores cloud-config user-data as a private yip document", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "#cloud-config\npackages: [vim]\nruncmd:\n  - echo done\n",
				mnt + "/meta-data":          "instance-id: vm1.agents\nlocal-hostname: vm1\n",
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())

			data, err := tfs.ReadFile(configFile)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).NotTo(HavePrefix("#cloud-config"))
			Expect(string(data)).To(ContainSubstring("hostname: vm1"))
			Expect(string(data)).To(ContainSubstring("- echo done"))

			info, err := tfs.Stat(configFile)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))

			id, err := tfs.ReadFile(pendingID)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(id)).To(Equal("vm1.agents\n"))

			By("not marking the instance as done before Finalize")
			Expect(vfs.Exists(tfs, appliedID)).To(BeFalse())

			By("logging what is not applied")
			Expect(logBuffer.String()).To(ContainSubstring("cloud-config: ignoring unsupported cloud-config key 'packages'"))
			Expect(logBuffer.String()).To(ContainSubstring("Using hostname 'vm1' from the datasource metadata"))
		})

		It("prefers the user-data hostname over the metadata one", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "#cloud-config\nhostname: custom\n",
				mnt + "/meta-data":          "instance-id: vm1\nlocal-hostname: vm1\n",
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())
			data, err := tfs.ReadFile(configFile)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(ContainSubstring("hostname: custom"))
			Expect(string(data)).NotTo(ContainSubstring("hostname: vm1"))
		})

		It("skips user-data already applied to the instance", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "#cloud-config\nhostname: foo\n",
				mnt + "/meta-data":          "instance-id: vm1\n",
				appliedID:                   "vm1\n",
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())
			Expect(vfs.Exists(tfs, configFile)).To(BeFalse())
			Expect(logBuffer.String()).To(ContainSubstring("was already applied to instance 'vm1', skipping it"))
		})

		It("applies user-data again if the instance changes", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "#cloud-config\nhostname: foo\n",
				mnt + "/meta-data":          "instance-id: vm2\n",
				appliedID:                   "vm1\n",
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())
			Expect(vfs.Exists(tfs, configFile)).To(BeTrue())
		})

		It("identifies the instance by the user-data checksum if there is no instance id", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "#cloud-config\nhostname: foo\n",
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())
			id, err := tfs.ReadFile(pendingID)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(id)).To(HavePrefix("sha256-"))
		})

		It("runs script user-data in the network stage", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "#!/bin/sh\necho hello\n",
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())

			script := filepath.Join(cloudinit.RunDir, "user-data-script")
			info, err := tfs.Stat(script)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o700)))

			data, err := tfs.ReadFile(configFile)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(ContainSubstring("network:"))
			Expect(string(data)).To(ContainSubstring(script))
		})

		It("accepts yip documents as user-data", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "stages:\n  boot:\n    - commands:\n        - echo yip\n",
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())
			data, err := tfs.ReadFile(configFile)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(ContainSubstring("- echo yip"))
		})

		It("leaves Ignition user-data to Ignition", func() {
			setup(map[string]any{
				"/dev/disk/by-label/config-2":            "",
				mnt + "/openstack/latest/user_data":      `{"ignition": {"version": "3.4.0"}}`,
				mnt + "/openstack/latest/meta_data.json": `{"uuid": "abc", "hostname": "vm2"}`,
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())
			Expect(vfs.Exists(tfs, configFile)).To(BeFalse())
			Expect(vfs.Exists(tfs, pendingID)).To(BeFalse())
			Expect(logBuffer.String()).To(ContainSubstring("looks like an Ignition configuration, ignoring it"))
		})

		It("ignores user-data in an unknown format", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "just some text",
			})
			Expect(cloudinit.New(s, cfg).Fetch()).To(Succeed())
			Expect(vfs.Exists(tfs, configFile)).To(BeFalse())
			Expect(logBuffer.String()).To(ContainSubstring("User-data is not in a supported format"))
		})

		It("fails with a detailed error on an invalid cloud-config", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data":          "#cloud-config\nruncmd: nope\n",
			})
			err := cloudinit.New(s, cfg).Fetch()
			Expect(err).To(MatchError(ContainSubstring(
				"processing user-data from NoCloud (/dev/disk/by-label/cidata): parsing cloud-config key 'runcmd' (line 2)")))
		})
	})

	Describe("RunStage", func() {
		It("runs the stage and its variants from the existing paths only", func() {
			setup(map[string]any{"/oem/90_custom.yaml": "stages: {}\n"})
			fake := &fakeRunner{}
			Expect(cloudinit.New(s, cfg, cloudinit.WithStageRunner(fake)).RunStage("boot")).To(Succeed())
			Expect(fake.stages).To(Equal([]string{"boot.before", "boot", "boot.after"}))
			Expect(fake.sources[0]).To(Equal([]string{"/oem"}))
		})

		It("does nothing if there is no configuration", func() {
			setup(map[string]any{})
			fake := &fakeRunner{}
			Expect(cloudinit.New(s, cfg, cloudinit.WithStageRunner(fake)).RunStage("boot")).To(Succeed())
			Expect(fake.stages).To(BeEmpty())
			Expect(logBuffer.String()).To(ContainSubstring("No cloud-init configuration found"))
		})

		It("logs errors and continues unless strict mode is set", func() {
			setup(map[string]any{"/oem/90_custom.yaml": "stages: {}\n"})
			fake := &fakeRunner{err: fmt.Errorf("command failed")}

			Expect(cloudinit.New(s, cfg, cloudinit.WithStageRunner(fake)).RunStage("boot")).To(Succeed())
			Expect(fake.stages).To(HaveLen(3))
			Expect(logBuffer.String()).To(ContainSubstring("running stage 'boot': command failed"))

			cfg.Strict = true
			err := cloudinit.New(s, cfg, cloudinit.WithStageRunner(fake)).RunStage("boot")
			Expect(err).To(MatchError(ContainSubstring("running stage 'boot.before': command failed")))
		})

		It("applies a fetched cloud-config with yip", func() {
			setup(map[string]any{
				"/dev/disk/by-label/cidata": "",
				mnt + "/user-data": "#cloud-config\nruncmd:\n  - [echo, '{{ literal }}']\n  - echo second\n" +
					"write_files:\n  - path: /must/not/exist\n    content: foo\n",
				mnt + "/meta-data": "instance-id: vm1\n",
				// A yip document as elemental-toolkit systems keep under /oem. Ownership is set to
				// the current user because tests do not run as root.
				"/oem/90_custom.yaml": fmt.Sprintf("stages:\n  network:\n    - files:\n        - path: /etc/test/file\n"+
					"          content: from oem\n          permissions: 0640\n          owner: %d\n          group: %d\n"+
					"      commands:\n        - echo oem\n", os.Getuid(), os.Getgid()),
			})
			ci := cloudinit.New(s, cfg)
			Expect(ci.Fetch()).To(Succeed())

			By("not applying early settings in a later stage")
			Expect(ci.RunStage(cloudinit.StageNetwork)).To(Succeed())
			Expect(vfs.Exists(tfs, "/must/not/exist")).To(BeFalse())

			By("writing files of yip documents")
			data, err := tfs.ReadFile("/etc/test/file")
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(Equal("from oem"))
			info, err := tfs.Stat("/etc/test/file")
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o640)))

			By("running commands through the shell, persistent paths first")
			Expect(runner.CmdsMatch([][]string{
				{"sh", "-c", "echo oem"},
				{"sh", "-c", "'echo' '{{ literal }}'"},
				{"sh", "-c", "echo second"},
			})).To(Succeed())
		})
	})

	Describe("Finalize", func() {
		It("records the instance the user-data was applied to", func() {
			setup(map[string]any{pendingID: "vm1\n"})
			Expect(cloudinit.New(s, cfg).Finalize()).To(Succeed())
			id, err := tfs.ReadFile(appliedID)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(id)).To(Equal("vm1\n"))
			Expect(logBuffer.String()).To(ContainSubstring("User-data for instance 'vm1' marked as applied"))
		})

		It("does nothing if no user-data was fetched", func() {
			setup(map[string]any{})
			Expect(cloudinit.New(s, cfg).Finalize()).To(Succeed())
			Expect(vfs.Exists(tfs, appliedID)).To(BeFalse())
		})
	})
})
