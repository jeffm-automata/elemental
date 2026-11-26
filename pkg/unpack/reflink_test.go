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

package unpack_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	ctrdmock "github.com/suse/elemental/v3/pkg/containerd/mock"
	"github.com/suse/elemental/v3/pkg/log"
	"github.com/suse/elemental/v3/pkg/sys"
	sysmock "github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
	"github.com/suse/elemental/v3/pkg/unpack"
)

// copyClone stands in for a reflink clone on file systems without reflinks: same result, by reading.
func copyClone(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0)
}

var _ = Describe("Reflink sync", Label("reflink"), func() {
	var tfs vfs.FS
	var s *sys.System
	var cleanup func()
	var src, dst string
	// Image layers have a constant modification time: equal size and time say nothing about the content.
	epoch := time.Unix(0, 0)

	write := func(root, rel, content string) {
		path := filepath.Join(root, rel)
		Expect(os.MkdirAll(filepath.Dir(path), 0755)).To(Succeed())
		Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())
		Expect(os.Chtimes(path, epoch, epoch)).To(Succeed())
	}
	read := func(root, rel string) string {
		data, err := os.ReadFile(filepath.Join(root, rel))
		Expect(err).NotTo(HaveOccurred())
		return string(data)
	}
	exists := func(root, rel string) bool {
		_, err := os.Lstat(filepath.Join(root, rel))
		return err == nil
	}

	BeforeEach(func() {
		var err error
		tfs, cleanup, err = sysmock.TestFS(nil)
		Expect(err).NotTo(HaveOccurred())
		s, err = sys.NewSystem(sys.WithFS(tfs), sys.WithLogger(log.New(log.WithDiscardAll())))
		Expect(err).NotTo(HaveOccurred())
		Expect(vfs.MkdirAll(tfs, "/image", vfs.DirPerm)).To(Succeed())
		Expect(vfs.MkdirAll(tfs, "/snapshot", vfs.DirPerm)).To(Succeed())
		src, err = tfs.RawPath("/image")
		Expect(err).NotTo(HaveOccurred())
		dst, err = tfs.RawPath("/snapshot")
		Expect(err).NotTo(HaveOccurred())

		// new image
		write(src, "usr/bin/tool", "new tool")
		write(src, "usr/bin/same", "same")
		write(src, "usr/bin/longer", "a longer file")
		write(src, "usr/bin/added", "added")
		write(src, "usr/lib/empty", "")
		write(src, "var/lib/data", "new data")
		write(src, "etc/conf", "new conf")
		// previous snapshot: 'tool' has the size and time of the new one and other content
		write(dst, "usr/bin/tool", "old tool")
		write(dst, "usr/bin/same", "same")
		write(dst, "usr/bin/longer", "short")
		write(dst, "usr/bin/removed", "removed")
		write(dst, "usr/lib/empty", "not empty")
		write(dst, "var/lib/data", "old data")
		write(dst, "etc/conf", "old conf")
	})
	AfterEach(func() {
		cleanup()
	})

	It("gives every file that exists on both sides the content and time of the image", func() {
		ok, cloned, dropped := unpack.ReflinkPrepare(context.Background(), s, "/image", "/snapshot", []string{"/var"}, copyClone)
		Expect(ok).To(BeTrue())
		Expect(cloned).To(Equal(5))
		Expect(dropped).To(Equal(0))

		Expect(read(dst, "usr/bin/tool")).To(Equal("new tool"))
		Expect(read(dst, "usr/bin/same")).To(Equal("same"))
		Expect(read(dst, "usr/bin/longer")).To(Equal("a longer file"))
		Expect(read(dst, "usr/lib/empty")).To(BeEmpty())
		Expect(read(dst, "etc/conf")).To(Equal("new conf"))
		for _, f := range []string{"usr/bin/tool", "usr/bin/same", "usr/bin/longer", "usr/lib/empty", "etc/conf"} {
			info, err := os.Lstat(filepath.Join(dst, f))
			Expect(err).NotTo(HaveOccurred())
			Expect(info.ModTime().Equal(epoch)).To(BeTrue(), f)
		}

		// excluded paths, new files and deletions are not its business
		Expect(read(dst, "var/lib/data")).To(Equal("old data"))
		Expect(exists(dst, "usr/bin/added")).To(BeFalse())
		Expect(exists(dst, "usr/bin/removed")).To(BeTrue())
	})

	It("does not clone into a file with several names and removes it instead", func() {
		Expect(os.Link(filepath.Join(dst, "usr/bin/tool"), filepath.Join(dst, "usr/bin/tool-alias"))).To(Succeed())
		write(src, "usr/bin/tool-alias", "an alias")

		ok, cloned, dropped := unpack.ReflinkPrepare(context.Background(), s, "/image", "/snapshot", []string{"/var"}, copyClone)
		Expect(ok).To(BeTrue())
		Expect(dropped).To(Equal(1))
		Expect(cloned).To(Equal(5))
		// The first of the two names is removed, the second one is a single name by then and is cloned. No name is
		// left with the old content.
		Expect(exists(dst, "usr/bin/tool")).To(BeFalse())
		Expect(read(dst, "usr/bin/tool-alias")).To(Equal("an alias"))
	})

	It("leaves other file types to rsync", func() {
		Expect(os.Remove(filepath.Join(dst, "usr/bin/tool"))).To(Succeed())
		Expect(os.Symlink("same", filepath.Join(dst, "usr/bin/tool"))).To(Succeed())
		Expect(os.Remove(filepath.Join(src, "usr/bin/longer"))).To(Succeed())
		Expect(os.Symlink("same", filepath.Join(src, "usr/bin/longer"))).To(Succeed())

		ok, cloned, dropped := unpack.ReflinkPrepare(context.Background(), s, "/image", "/snapshot", []string{"/var"}, copyClone)
		Expect(ok).To(BeTrue())
		Expect(cloned).To(Equal(3))
		Expect(dropped).To(Equal(0))
		Expect(read(dst, "usr/bin/same")).To(Equal("same"))
		target, err := os.Readlink(filepath.Join(dst, "usr/bin/tool"))
		Expect(err).NotTo(HaveOccurred())
		Expect(target).To(Equal("same"))
		Expect(read(dst, "usr/bin/longer")).To(Equal("short"))
	})

	It("reports that checksums are needed if the first clone fails", func() {
		calls := 0
		failing := func(_, _ string) error { calls++; return fmt.Errorf("invalid cross-device link") }
		ok, cloned, _ := unpack.ReflinkPrepare(context.Background(), s, "/image", "/snapshot", []string{"/var"}, failing)
		Expect(ok).To(BeFalse())
		Expect(cloned).To(Equal(0))
		Expect(calls).To(Equal(1))
		// nothing else was touched
		Expect(read(dst, "usr/bin/tool")).To(Equal("old tool"))
	})

	It("removes a file whose clone fails after others were cloned", func() {
		calls := 0
		flaky := func(a, b string) error {
			calls++
			if calls == 2 {
				return fmt.Errorf("operation not permitted")
			}
			return copyClone(a, b)
		}
		ok, cloned, dropped := unpack.ReflinkPrepare(context.Background(), s, "/image", "/snapshot", []string{"/var"}, flaky)
		Expect(ok).To(BeTrue())
		Expect(cloned).To(Equal(4))
		Expect(dropped).To(Equal(1))
		// every file is either the new one or gone; none kept old content
		for _, f := range []string{"usr/bin/tool", "usr/bin/same", "usr/bin/longer", "usr/lib/empty", "etc/conf"} {
			if exists(dst, f) {
				Expect(read(dst, f)).To(Equal(read(src, f)), f)
			}
		}
	})

	It("refuses exclude patterns it cannot evaluate", func() {
		ok, _, _ := unpack.ReflinkPrepare(context.Background(), s, "/image", "/snapshot", []string{"/var/*"}, copyClone)
		Expect(ok).To(BeFalse())
		Expect(read(dst, "usr/bin/tool")).To(Equal("old tool"))
	})

	It("stops when the context is cancelled", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ok, _, _ := unpack.ReflinkPrepare(ctx, s, "/image", "/snapshot", nil, copyClone)
		Expect(ok).To(BeFalse())
	})

	It("clones for real where the file system can, and says so where it cannot", func() {
		ok, cloned, dropped := unpack.ReflinkPrepare(context.Background(), s, "/image", "/snapshot", []string{"/var"}, unpack.Ficlone)
		GinkgoWriter.Printf("reflink clone on %s: usable=%t cloned=%d dropped=%d\n", dst, ok, cloned, dropped)
		if !ok {
			Expect(cloned).To(Equal(0))
			return
		}
		Expect(cloned + dropped).To(Equal(5))
		for _, f := range []string{"usr/bin/tool", "usr/bin/same", "usr/bin/longer", "usr/lib/empty", "etc/conf"} {
			if exists(dst, f) {
				Expect(read(dst, f)).To(Equal(read(src, f)), f)
			}
		}
	})

	It("mirrors an image unpacked in containerd whatever the file system can do", func() {
		Expect(os.Setenv(unpack.CtrdSockEnv, "/run/containerd/containerd.sock")).To(Succeed())
		Expect(vfs.MkdirAll(tfs, "/run/containerd", vfs.DirPerm)).To(Succeed())
		Expect(tfs.WriteFile("/run/containerd/containerd.sock", []byte{}, vfs.FilePerm)).To(Succeed())
		ctrd := &ctrdmock.ContainerdMock{MntRootFS: "/image"}
		ctrd.Img.Digest = "imageID"

		unpacker := unpack.NewOCIUnpacker(s, "registry.invalid./os:1", unpack.WithContainerd(ctrd), unpack.WithLocalOCI(true))
		digest, err := unpacker.SynchedUnpack(context.Background(), "/snapshot", []string{"/var"}, []string{"/var"})
		Expect(err).NotTo(HaveOccurred())
		Expect(digest).To(Equal("imageID"))

		for _, f := range []string{"usr/bin/tool", "usr/bin/same", "usr/bin/longer", "usr/bin/added", "usr/lib/empty", "etc/conf"} {
			Expect(read(dst, f)).To(Equal(read(src, f)), f)
		}
		Expect(exists(dst, "usr/bin/removed")).To(BeFalse())
		Expect(read(dst, "var/lib/data")).To(Equal("old data"))
	})

	It("can be switched off", func() {
		Expect(unpack.ReflinkSyncEnabled()).To(BeTrue())
		Expect(os.Setenv(unpack.ReflinkSyncEnv, "false")).To(Succeed())
		DeferCleanup(func() { Expect(os.Unsetenv(unpack.ReflinkSyncEnv)).To(Succeed()) })
		Expect(unpack.ReflinkSyncEnabled()).To(BeFalse())
	})

	It("synchronizes without checksums only", func() {
		Expect(unpack.QuickCheckFlags()).NotTo(ContainElement("--checksum"))
		Expect(unpack.QuickCheckFlags()).To(ContainElements("--archive", "--xattrs", "--acls"))
	})

	Describe("overlay mounts", func() {
		const mountInfo = `36 1 0:32 /@/.snapshots/1/snapshot / ro,relatime shared:1 - btrfs /dev/vda3 ro,seclabel,subvolid=266
901 36 0:71 / /tmp/elemental-mnt-1 ro,relatime shared:400 - overlay overlay ro,seclabel,lowerdir=/var/lib/c/snapshots/12/fs:/var/lib/c/snapshots/11/fs
902 36 0:32 /@/var/lib/c/snapshots/11/fs /tmp/elemental-mnt-2 ro,relatime shared:401 - btrfs /dev/vda3 rw,seclabel
903 36 0:72 / /tmp/elemental-mnt-3 ro,relatime - overlay overlay ro,lowerdir=l/AB:l/CD
904 36 0:73 / /tmp/with\040space ro - overlay overlay ro,lowerdir=/a:/b,index=off
`
		It("names the layers of an overlay mount, topmost first", func() {
			layers, err := unpack.ParseOverlayLayers(mountInfo, "/tmp/elemental-mnt-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(layers).To(Equal([]string{"/var/lib/c/snapshots/12/fs", "/var/lib/c/snapshots/11/fs"}))

			layers, err = unpack.ParseOverlayLayers(mountInfo, "/tmp/with space")
			Expect(err).NotTo(HaveOccurred())
			Expect(layers).To(Equal([]string{"/a", "/b"}))
		})
		It("has no layers for a bind mount or a plain directory", func() {
			layers, err := unpack.ParseOverlayLayers(mountInfo, "/tmp/elemental-mnt-2")
			Expect(err).NotTo(HaveOccurred())
			Expect(layers).To(BeNil())
			layers, err = unpack.ParseOverlayLayers(mountInfo, "/tmp/not-a-mount-point")
			Expect(err).NotTo(HaveOccurred())
			Expect(layers).To(BeNil())
		})
		It("fails for layers it cannot name", func() {
			_, err := unpack.ParseOverlayLayers(mountInfo, "/tmp/elemental-mnt-3")
			Expect(err).To(HaveOccurred())
		})
		It("finds a file in the topmost layer that has it and checks it is the same file", func() {
			upper := filepath.Join(dst, "layers/2")
			lower := filepath.Join(dst, "layers/1")
			write(lower, "usr/bin/a", "lower a")
			write(lower, "usr/bin/b", "lower b")
			write(upper, "usr/bin/b", "upper b")
			layers := []string{upper, lower}

			infoA, err := os.Lstat(filepath.Join(lower, "usr/bin/a"))
			Expect(err).NotTo(HaveOccurred())
			infoB, err := os.Lstat(filepath.Join(upper, "usr/bin/b"))
			Expect(err).NotTo(HaveOccurred())

			Expect(unpack.LayerFile(layers, "/usr/bin/a", infoA)).To(Equal(filepath.Join(lower, "usr/bin/a")))
			Expect(unpack.LayerFile(layers, "/usr/bin/b", infoB)).To(Equal(filepath.Join(upper, "usr/bin/b")))

			// the overlay shows another file than the topmost layer has: not usable
			lowerB, err := os.Lstat(filepath.Join(lower, "usr/bin/b"))
			Expect(err).NotTo(HaveOccurred())
			_, err = unpack.LayerFile(layers, "/usr/bin/b", lowerB)
			Expect(err).To(HaveOccurred())
			_, err = unpack.LayerFile(layers, "/usr/bin/none", infoA)
			Expect(err).To(HaveOccurred())
		})
	})
})
