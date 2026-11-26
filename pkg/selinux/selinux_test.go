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

package selinux_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suse/elemental/v3/pkg/log"
	"github.com/suse/elemental/v3/pkg/selinux"
	"github.com/suse/elemental/v3/pkg/sys"
	sysmock "github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	semanageConf = "/etc/selinux/semanage.conf"
	etcStore     = "/etc/selinux/targeted/active"
)

func TestSELinuxSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "SELinux test suite")
}

var _ = Describe("Selinux", Label("selinux"), func() {
	var runner *sysmock.Runner
	var mounter *sysmock.Mounter
	var syscall *sysmock.Syscall
	var fs vfs.FS
	var cleanup func()
	var s *sys.System
	var root string
	var contextFile string
	var buffer *bytes.Buffer

	BeforeEach(func() {
		var err error
		syscall = &sysmock.Syscall{}
		buffer = &bytes.Buffer{}
		runner = sysmock.NewRunner()
		mounter = sysmock.NewMounter()
		fs, cleanup, err = sysmock.TestFS(nil)
		Expect(err).ToNot(HaveOccurred())
		logger := log.New(log.WithBuffer(buffer))
		logger.SetLevel(log.DebugLevel())
		s, err = sys.NewSystem(
			sys.WithMounter(mounter), sys.WithRunner(runner),
			sys.WithFS(fs), sys.WithLogger(logger),
			sys.WithSyscall(syscall),
		)
		Expect(err).NotTo(HaveOccurred())
		root = "/some/root"
		Expect(vfs.MkdirAll(fs, root, vfs.DirPerm)).To(Succeed())
		contextFile = filepath.Join(root, selinux.SelinuxTargetedContextFile)
		Expect(vfs.MkdirAll(fs, filepath.Dir(contextFile), vfs.DirPerm)).To(Succeed())
		Expect(vfs.MkdirAll(fs, filepath.Dir(selinux.SelinuxTargetedContextFile), vfs.DirPerm)).To(Succeed())
	})
	AfterEach(func() {
		cleanup()
	})
	It("relabels the given path for the targeted context, no shared paths no .autorelabel file", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(selinux.SystemRelabel(context.Background(), s, root, []string{root + "/etc"}, nil)).To(Succeed())
		Expect(runner.CmdsMatch([][]string{{
			"setfiles", "-i", "-r", "/some/root", "-F", "-e", "/some/root/etc",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root",
		}, {
			"setfiles", "-i", "-r", "/some/root",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root/etc",
		}})).To(Succeed())
		Expect(vfs.Exists(s.FS(), filepath.Join(root, "/etc/selinux/.autorelabel"))).To(BeFalse())
	})
	It("names the nested snapshots subvolume of a snapshotted path, which setfiles would not cross into", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(vfs.MkdirAll(fs, root+"/etc/.snapshots/1/snapshot", vfs.DirPerm)).To(Succeed())
		Expect(vfs.MkdirAll(fs, root+"/home", vfs.DirPerm)).To(Succeed())
		Expect(selinux.SystemRelabel(context.Background(), s, root, []string{root + "/etc", root + "/home"}, nil)).To(Succeed())
		Expect(runner.CmdsMatch([][]string{{
			"setfiles", "-i", "-r", "/some/root", "-F", "-e", "/some/root/etc", "-e", "/some/root/home",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root",
		}, {
			// the snapshots directory is a path of its own; the snapshots inside it are not named
			"setfiles", "-i", "-r", "/some/root",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root/etc", "/some/root/etc/.snapshots",
		}, {
			// a snapshotted path without snapshots directory is relabelled as before
			"setfiles", "-i", "-r", "/some/root",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root/home",
		}})).To(Succeed())
		// CmdsMatch compares prefixes: nothing may follow the path of a volume without snapshots directory
		cmds := runner.GetCmds()
		Expect(cmds[2][len(cmds[2])-1]).To(Equal("/some/root/home"))
		Expect(vfs.Exists(s.FS(), filepath.Join(root, "/etc/selinux/.autorelabel"))).To(BeFalse())
	})
	It("does not take a file called .snapshots for a snapshots subvolume", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(fs.WriteFile(root+"/etc/.snapshots", []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(selinux.SystemRelabel(context.Background(), s, root, []string{root + "/etc"}, nil)).To(Succeed())
		Expect(runner.CmdsMatch([][]string{{
			"setfiles", "-i", "-r", "/some/root", "-F", "-e", "/some/root/etc",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root",
		}, {
			"setfiles", "-i", "-r", "/some/root",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root/etc",
		}})).To(Succeed())
		// CmdsMatch compares prefixes: the path must be the last argument
		cmds := runner.GetCmds()
		Expect(cmds[1][len(cmds[1])-1]).To(Equal("/some/root/etc"))
	})
	It("relabels the given path for the targeted context, including shared paths sets the .autorelabel file", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(selinux.SystemRelabel(context.Background(), s, root, nil, []string{root + "/var"})).To(Succeed())
		Expect(runner.CmdsMatch([][]string{{
			"setfiles", "-i", "-r", "/some/root", "-F", "-e", "/some/root/var",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root",
		}})).To(Succeed())
		Expect(vfs.Exists(s.FS(), filepath.Join(root, "/etc/selinux/.autorelabel"))).To(BeTrue())
	})
	It("does nothing if the context is not found", func() {
		Expect(selinux.SystemRelabel(context.Background(), s, root, nil, nil)).To(Succeed())
		Expect(runner.CmdsMatch([][]string{{}}))
		Expect(buffer.String()).To(ContainSubstring("no context found"))
	})
	It("errors out if setfiles call fails", func() {
		runner.SideEffect = func(cmd string, args ...string) ([]byte, error) {
			if cmd == "setfiles" {
				return []byte{}, fmt.Errorf("setfiles failed")
			}
			return []byte{}, nil
		}
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(selinux.SystemRelabel(context.Background(), s, root, []string{}, []string{})).
			To(MatchError(ContainSubstring("setfiles failed")))
	})
	It("relabels the given paths for the targeted context in a chroot env", func() {
		Expect(fs.WriteFile(selinux.SelinuxTargetedContextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(vfs.MkdirAll(fs, "/partition/var", vfs.DirPerm)).To(Succeed())
		Expect(selinux.ChrootedSystemRelabel(
			context.Background(), s, root, []string{"/etc"}, []string{"/var"}),
		).To(Succeed())
		Expect(runner.CmdsMatch([][]string{
			{"setfiles", "-i", "-F", "-e", "/etc", "-e", "/var", "/etc/selinux/targeted/contexts/files/file_contexts", "/"},
			{"setfiles", "-i", "/etc/selinux/targeted/contexts/files/file_contexts", "/etc"},
			{"sync"},
		})).To(Succeed())
	})
	It("names the nested snapshots subvolume of a snapshotted path in a chroot env", func() {
		Expect(fs.WriteFile(selinux.SelinuxTargetedContextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(vfs.MkdirAll(fs, "/partition/var", vfs.DirPerm)).To(Succeed())
		Expect(vfs.MkdirAll(fs, "/etc/.snapshots", vfs.DirPerm)).To(Succeed())
		Expect(selinux.ChrootedSystemRelabel(
			context.Background(), s, root, []string{"/etc"}, []string{"/var"}),
		).To(Succeed())
		Expect(runner.CmdsMatch([][]string{
			{"setfiles", "-i", "-F", "-e", "/etc", "-e", "/var", "/etc/selinux/targeted/contexts/files/file_contexts", "/"},
			{"setfiles", "-i", "/etc/selinux/targeted/contexts/files/file_contexts", "/etc", "/etc/.snapshots"},
			{"sync"},
		})).To(Succeed())
	})
	It("does not ask for a relabel at boot, and withdraws a request, when told so", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		autorelabel := filepath.Join(root, "/etc/selinux/.autorelabel")
		Expect(fs.WriteFile(autorelabel, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(selinux.SystemRelabel(
			context.Background(), s, root, nil, []string{root + "/var"}, selinux.WithoutAutoRelabel()),
		).To(Succeed())
		// the shared path is labelled by the upgrade itself then, without force and after the root tree
		Expect(runner.CmdsMatch([][]string{{
			"setfiles", "-i", "-r", "/some/root", "-F", "-e", "/some/root/var",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root",
		}, {
			"setfiles", "-i", "-r", "/some/root",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root/var",
		}})).To(Succeed())
		Expect(vfs.Exists(s.FS(), autorelabel)).To(BeFalse())
		Expect(buffer.String()).To(ContainSubstring("Applying SE Linux labels to shared RW paths"))
		runner.ClearCmds()

		// nothing to withdraw is fine too
		Expect(selinux.SystemRelabel(
			context.Background(), s, root, nil, []string{root + "/var"}, selinux.WithoutAutoRelabel()),
		).To(Succeed())
		Expect(vfs.Exists(s.FS(), autorelabel)).To(BeFalse())
	})
	It("labels every shared path in one pass when no relabel at boot is asked for, snapshotted paths before", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(vfs.MkdirAll(fs, root+"/etc/.snapshots", vfs.DirPerm)).To(Succeed())
		Expect(selinux.SystemRelabel(
			context.Background(), s, root, []string{root + "/etc"}, []string{root + "/var", root + "/opt"},
			selinux.WithoutAutoRelabel()),
		).To(Succeed())
		Expect(runner.CmdsMatch([][]string{{
			"setfiles", "-i", "-r", "/some/root", "-F", "-e", "/some/root/etc", "-e", "/some/root/var", "-e", "/some/root/opt",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root",
		}, {
			"setfiles", "-i", "-r", "/some/root",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root/etc", "/some/root/etc/.snapshots",
		}, {
			"setfiles", "-i", "-r", "/some/root",
			"/some/root/etc/selinux/targeted/contexts/files/file_contexts", "/some/root/var", "/some/root/opt",
		}})).To(Succeed())
		// never forced: customized labels in shared paths stay
		cmds := runner.GetCmds()
		Expect(cmds[2]).NotTo(ContainElement("-F"))
		Expect(vfs.Exists(s.FS(), filepath.Join(root, "/etc/selinux/.autorelabel"))).To(BeFalse())
	})
	It("does not label shared paths itself when the relabel at boot is asked for", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		Expect(selinux.SystemRelabel(
			context.Background(), s, root, []string{root + "/etc"}, []string{root + "/var"}),
		).To(Succeed())
		Expect(runner.GetCmds()).To(HaveLen(2))
		Expect(buffer.String()).NotTo(ContainSubstring("Applying SE Linux labels to shared RW paths"))
		Expect(vfs.Exists(s.FS(), filepath.Join(root, "/etc/selinux/.autorelabel"))).To(BeTrue())
	})
	It("asks for the relabel at boot after all if the shared paths cannot be labelled", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		runner.SideEffect = func(cmd string, args ...string) ([]byte, error) {
			if cmd == "setfiles" && args[len(args)-1] == root+"/var" {
				return []byte{}, fmt.Errorf("setfiles failed on a shared path")
			}
			return []byte{}, nil
		}
		autorelabel := filepath.Join(root, "/etc/selinux/.autorelabel")
		Expect(selinux.SystemRelabel(
			context.Background(), s, root, nil, []string{root + "/var"}, selinux.WithoutAutoRelabel()),
		).To(Succeed())
		Expect(vfs.Exists(s.FS(), autorelabel)).To(BeTrue())
		Expect(buffer.String()).To(ContainSubstring("they are relabelled at the next boot"))
	})
	It("fails without touching shared paths if the root tree cannot be labelled", func() {
		Expect(fs.WriteFile(contextFile, []byte{}, vfs.FilePerm)).To(Succeed())
		runner.SideEffect = func(cmd string, args ...string) ([]byte, error) {
			if cmd == "setfiles" && args[len(args)-1] == root {
				return []byte{}, fmt.Errorf("setfiles failed on the root")
			}
			return []byte{}, nil
		}
		Expect(selinux.SystemRelabel(
			context.Background(), s, root, nil, []string{root + "/var"}, selinux.WithoutAutoRelabel()),
		).To(MatchError(ContainSubstring("setfiles failed on the root")))
		Expect(runner.GetCmds()).To(HaveLen(1))
	})
	Describe("labels of shared paths", func() {
		const oldRoot = "/old/root"
		const contexts = "/etc/selinux/targeted/contexts/files"
		write := func(tree, name, content string) {
			path := filepath.Join(tree, name)
			Expect(vfs.MkdirAll(fs, filepath.Dir(path), vfs.DirPerm)).To(Succeed())
			Expect(fs.WriteFile(path, []byte(content), vfs.FilePerm)).To(Succeed())
		}
		BeforeEach(func() {
			for _, tree := range []string{oldRoot, root} {
				write(tree, contexts+"/file_contexts", "/var(/.*)? system_u:object_r:var_t:s0\n")
				write(tree, contexts+"/file_contexts.homedirs", "homedirs")
				write(tree, contexts+"/file_contexts.subs_dist", "subs")
				write(tree, "/etc/selinux/config", "SELINUX=enforcing\nSELINUXTYPE=targeted\n")
			}
			write(oldRoot, "/etc/selinux/.relabelled", "")
		})
		It("are still valid if no context definition changed", func() {
			// compiled companions are derived, they may differ
			write(oldRoot, contexts+"/file_contexts.bin", "compiled by the old tools")
			write(root, contexts+"/file_contexts.bin", "compiled by the new tools")
			valid, reason := selinux.SharedLabelsStillValid(s, oldRoot, root)
			Expect(reason).To(BeEmpty())
			Expect(valid).To(BeTrue())
		})
		It("are not valid if a context definition changed", func() {
			write(root, contexts+"/file_contexts", "/var(/.*)? system_u:object_r:other_t:s0\n")
			valid, reason := selinux.SharedLabelsStillValid(s, oldRoot, root)
			Expect(valid).To(BeFalse())
			Expect(reason).To(ContainSubstring("file_contexts' changed"))
		})
		It("are not valid if the new system has a definition file the old one has not, or the reverse", func() {
			write(root, contexts+"/file_contexts.local", "/srv/x system_u:object_r:var_t:s0\n")
			valid, reason := selinux.SharedLabelsStillValid(s, oldRoot, root)
			Expect(valid).To(BeFalse())
			Expect(reason).To(ContainSubstring("file_contexts.local' of the running system"))

			Expect(fs.Remove(filepath.Join(root, contexts, "file_contexts.local"))).To(Succeed())
			Expect(fs.Remove(filepath.Join(root, contexts, "file_contexts.homedirs"))).To(Succeed())
			valid, reason = selinux.SharedLabelsStillValid(s, oldRoot, root)
			Expect(valid).To(BeFalse())
			Expect(reason).To(ContainSubstring("file_contexts.homedirs' of the new system"))
		})
		It("are not valid if the SE Linux configuration changed", func() {
			write(root, "/etc/selinux/config", "SELINUX=permissive\nSELINUXTYPE=targeted\n")
			valid, reason := selinux.SharedLabelsStillValid(s, oldRoot, root)
			Expect(valid).To(BeFalse())
			Expect(reason).To(ContainSubstring("config' changed"))
		})
		It("are not valid while a relabel is pending on the running system", func() {
			write(oldRoot, "/etc/selinux/.autorelabel", "")
			valid, reason := selinux.SharedLabelsStillValid(s, oldRoot, root)
			Expect(valid).To(BeFalse())
			Expect(reason).To(ContainSubstring("pending"))
		})
		It("are not valid if the running system never completed a relabel", func() {
			Expect(fs.Remove(filepath.Join(oldRoot, "/etc/selinux/.relabelled"))).To(Succeed())
			valid, reason := selinux.SharedLabelsStillValid(s, oldRoot, root)
			Expect(valid).To(BeFalse())
			Expect(reason).To(ContainSubstring("no completed relabel"))
		})
		It("are not valid if there are no file contexts at all", func() {
			Expect(fs.Remove(filepath.Join(oldRoot, contexts, "file_contexts"))).To(Succeed())
			Expect(fs.Remove(filepath.Join(root, contexts, "file_contexts"))).To(Succeed())
			valid, _ := selinux.SharedLabelsStillValid(s, oldRoot, root)
			Expect(valid).To(BeFalse())

			valid, _ = selinux.SharedLabelsStillValid(s, "/no/such/root", root)
			Expect(valid).To(BeFalse())
		})
	})
	It("does nothing if the context is not found in chroot", func() {
		Expect(selinux.ChrootedSystemRelabel(
			context.Background(), s, root, nil, nil),
		).To(Succeed())
		Expect(runner.CmdsMatch([][]string{{}}))
		Expect(buffer.String()).To(ContainSubstring("no context found"))
	})
})

var _ = Describe("Policy refresh", Label("selinux"), func() {
	var runner *sysmock.Runner
	var fs vfs.FS
	var cleanup func()
	var s *sys.System
	var buffer *bytes.Buffer
	root := "/some/root"

	BeforeEach(func() {
		var err error
		buffer = &bytes.Buffer{}
		runner = sysmock.NewRunner()
		fs, cleanup, err = sysmock.TestFS(nil)
		Expect(err).ToNot(HaveOccurred())
		logger := log.New(log.WithBuffer(buffer))
		logger.SetLevel(log.DebugLevel())
		s, err = sys.NewSystem(
			sys.WithMounter(sysmock.NewMounter()), sys.WithRunner(runner),
			sys.WithFS(fs), sys.WithLogger(logger),
			sys.WithSyscall(&sysmock.Syscall{}),
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(vfs.MkdirAll(fs, root, vfs.DirPerm)).To(Succeed())
		Expect(vfs.MkdirAll(fs, etcStore+"/modules", 0o700)).To(Succeed())
		Expect(fs.WriteFile(semanageConf, []byte("module-store = direct\nstore-root=/etc/selinux\n"), vfs.FilePerm)).To(Succeed())
	})
	AfterEach(func() {
		cleanup()
	})

	It("skips refresh when no store-root has been provided", func() {
		Expect(fs.WriteFile(semanageConf, []byte("module-store = direct\n"), vfs.FilePerm)).To(Succeed())
		Expect(selinux.ChrootedRefreshPolicy(context.Background(), s, root, selinux.SelinuxTargetedPolicyType)).To(Succeed())

		Expect(runner.IncludesCmds([][]string{{"semodule"}})).NotTo(Succeed())
		Expect(buffer.String()).To(ContainSubstring("skipping policy refresh"))
	})

	It("skips refresh when semanage.conf is missing", func() {
		Expect(fs.Remove(semanageConf)).To(Succeed())
		Expect(selinux.ChrootedRefreshPolicy(context.Background(), s, root, selinux.SelinuxTargetedPolicyType)).To(Succeed())
		Expect(runner.IncludesCmds([][]string{{"semodule"}})).NotTo(Succeed())
	})

	It("skips refresh when there is no active policy store", func() {
		Expect(fs.RemoveAll(etcStore)).To(Succeed())
		Expect(selinux.ChrootedRefreshPolicy(context.Background(), s, root, selinux.SelinuxTargetedPolicyType)).To(Succeed())
		Expect(runner.IncludesCmds([][]string{{"semodule"}})).NotTo(Succeed())
		Expect(buffer.String()).To(ContainSubstring("No SE Linux policy store found"))
	})

	It("fails on semodule command error", func() {
		runner.SideEffect = func(cmd string, _ ...string) ([]byte, error) {
			if cmd == "semodule" {
				return []byte{}, fmt.Errorf("semodule failed")
			}
			return []byte{}, nil
		}
		Expect(selinux.ChrootedRefreshPolicy(context.Background(), s, root, selinux.SelinuxTargetedPolicyType)).To(MatchError(ContainSubstring("semodule failed")))
	})
})
