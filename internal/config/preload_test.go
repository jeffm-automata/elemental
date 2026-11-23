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
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	containerregistry "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suse/elemental/v3/internal/image"
	"github.com/suse/elemental/v3/internal/image/install"
	"github.com/suse/elemental/v3/internal/image/kubernetes"
	"github.com/suse/elemental/v3/pkg/log"
	"github.com/suse/elemental/v3/pkg/manifest/api/core"
	"github.com/suse/elemental/v3/pkg/manifest/resolver"
	"github.com/suse/elemental/v3/pkg/sys"
	sysmock "github.com/suse/elemental/v3/pkg/sys/mock"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

var _ = Describe("Preloaded container images", func() {
	const (
		runtimeImage = "docker.io/rancher/rke2-runtime:v1.37.1-rke2r1"
		calicoImage  = "docker.io/rancher/hardened-calico:v3.32.2"
		agentImage   = "rancher/rancher-agent:v2.15.3"
	)

	var output = Output{RootPath: "/_out"}

	var system *sys.System
	var fs vfs.FS
	var cleanup func()
	var conf *image.Configuration
	var rm *resolver.ResolvedManifest
	var fetched []string
	var images map[string]containerregistry.Image
	var manager *Manager

	// entries reads a tar archive into a map of entry name to content
	entries := func(archive string) map[string]string {
		file, err := fs.Open(archive)
		Expect(err).NotTo(HaveOccurred())
		defer file.Close()

		content := map[string]string{}
		reader := tar.NewReader(file)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				return content
			}
			Expect(err).NotTo(HaveOccurred())

			data, err := io.ReadAll(reader)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).NotTo(HaveKey(header.Name), "an entry is archived once")
			content[header.Name] = string(data)
		}
	}

	BeforeEach(func() {
		var err error

		fs, cleanup, err = sysmock.TestFS(nil)
		Expect(err).NotTo(HaveOccurred())

		system, err = sys.NewSystem(
			sys.WithLogger(log.New(log.WithDiscardAll())),
			sys.WithFS(fs),
		)
		Expect(err).NotTo(HaveOccurred())

		installDir := filepath.Join(output.OverlaysDir(), image.KubernetesInstallPath())
		Expect(vfs.MkdirAll(fs, installDir, vfs.DirPerm)).To(Succeed())
		arch := system.Platform().GolangArch
		Expect(fs.WriteFile(filepath.Join(installDir, kubernetes.ImageListName("core", arch)), []byte(runtimeImage+"\n"), vfs.FilePerm)).To(Succeed())
		Expect(fs.WriteFile(filepath.Join(installDir, kubernetes.ImageListName("canal", arch)), []byte(calicoImage+"\n"), vfs.FilePerm)).To(Succeed())

		rm = &resolver.ResolvedManifest{
			CorePlatform: &core.ReleaseManifest{
				Components: core.Components{
					Kubernetes: &core.Kubernetes{Version: "v1.37.1+rke2r1", Image: "registry.example.com/rke2:1.37"},
				},
			},
		}

		conf = &image.Configuration{
			Installation: install.Installation{
				RAW: install.RAW{
					Preinstalled:  true,
					PreloadImages: &install.PreloadImages{Images: []string{agentImage}},
				},
			},
		}

		fetched = nil
		images = map[string]containerregistry.Image{}
		manager = NewManager(system, nil,
			WithImageFetchFunc(func(_ context.Context, imageRef string) (containerregistry.Image, error) {
				fetched = append(fetched, imageRef)
				if img, ok := images[imageRef]; ok {
					return img, nil
				}
				img, err := random.Image(64, 2)
				images[imageRef] = img
				return img, err
			}),
			WithUnpackFunc(func(_ context.Context, imageRef, destDir string) error {
				Expect(imageRef).To(Equal(runtimeImage))
				for file, content := range map[string]string{
					"bin/containerd":         "containerd",
					"charts/rke2-canal.yaml": "chart",
					"etc/ignored":            "ignored",
				} {
					Expect(vfs.MkdirAll(fs, filepath.Join(destDir, filepath.Dir(file)), vfs.DirPerm)).To(Succeed())
					Expect(fs.WriteFile(filepath.Join(destDir, file), []byte(content), 0o755)).To(Succeed())
				}
				return fs.Symlink("containerd", filepath.Join(destDir, "bin", "ctr"))
			}),
		)
	})

	AfterEach(func() {
		cleanup()
	})

	It("does nothing when no images are to be preloaded", func() {
		conf.Installation.RAW.PreloadImages = nil

		Expect(manager.preloadContainerImages(context.Background(), conf, rm, output)).To(Succeed())

		Expect(fetched).To(BeEmpty())
		exists, _ := vfs.Exists(fs, output.PreloadStoreDir())
		Expect(exists).To(BeFalse())
	})

	It("archives the images of the release and the listed ones under the names containerd knows them by", func() {
		Expect(manager.preloadContainerImages(context.Background(), conf, rm, output)).To(Succeed())

		Expect(fetched).To(Equal([]string{calicoImage, agentImage, runtimeImage}))

		archive := entries(output.PreloadImagesArchive())
		Expect(archive).To(HaveKeyWithValue("oci-layout", `{"imageLayoutVersion":"1.0.0"}`))

		var index imageArchiveIndex
		Expect(json.Unmarshal([]byte(archive["index.json"]), &index)).To(Succeed())
		Expect(index.SchemaVersion).To(Equal(2))
		Expect(index.Manifests).To(HaveLen(3))

		var names []string
		for _, manifest := range index.Manifests {
			names = append(names, manifest.Annotations["io.containerd.image.name"])
		}
		Expect(names).To(Equal([]string{calicoImage, "docker.io/rancher/rancher-agent:v2.15.3", runtimeImage}))

		// Every manifest, configuration and layer is in the archive, as the registry serves it.
		for ref, img := range images {
			digest, err := img.Digest()
			Expect(err).NotTo(HaveOccurred())
			raw, err := img.RawManifest()
			Expect(err).NotTo(HaveOccurred())
			Expect(archive).To(HaveKeyWithValue("blobs/sha256/"+digest.Hex, string(raw)), ref)

			config, err := img.ConfigName()
			Expect(err).NotTo(HaveOccurred())
			Expect(archive).To(HaveKey("blobs/sha256/" + config.Hex))

			layers, err := img.Layers()
			Expect(err).NotTo(HaveOccurred())
			for _, layer := range layers {
				layerDigest, err := layer.Digest()
				Expect(err).NotTo(HaveOccurred())
				Expect(archive).To(HaveKey("blobs/sha256/" + layerDigest.Hex))
			}
		}
		Expect(archive).To(HaveLen(2 + 3*4))
	})

	It("archives the Kubernetes runtime below the directory RKE2 looks for it in", func() {
		Expect(manager.preloadContainerImages(context.Background(), conf, rm, output)).To(Succeed())

		// The name a node of RKE2 v1.37.1+rke2r1 gave the directory.
		const dataDir = "v1.37.1-rke2r1-fce60b3fe2ba"

		file, err := fs.Open(output.PreloadRuntimeArchive())
		Expect(err).NotTo(HaveOccurred())
		defer file.Close()

		found := map[string]*tar.Header{}
		content := map[string]string{}
		reader := tar.NewReader(file)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			Expect(err).NotTo(HaveOccurred())
			found[header.Name] = header
			data, err := io.ReadAll(reader)
			Expect(err).NotTo(HaveOccurred())
			content[header.Name] = string(data)
		}

		Expect(found).To(HaveLen(6))
		// What RKE2 v1.37.2+rke2r1 writes after it has extracted the image itself, for its own version.
		Expect(content).To(HaveKeyWithValue(dataDir+"/.extracted", "index.docker.io/rancher/rke2-runtime:v1.37.1-rke2r1"))
		Expect(found).To(HaveKey(dataDir + "/bin"))
		Expect(found).To(HaveKey(dataDir + "/charts"))
		Expect(found).To(HaveKey(dataDir + "/charts/rke2-canal.yaml"))
		Expect(found[dataDir+"/bin/containerd"].Mode & 0o111).NotTo(BeZero())
		Expect(found[dataDir+"/bin/containerd"].Uid).To(BeZero())
		Expect(found[dataDir+"/bin/ctr"].Linkname).To(Equal("containerd"))

		// Nothing is left behind next to the two archives.
		left, err := fs.ReadDir(output.PreloadStoreDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(left).To(HaveLen(2))
	})

	It("stores an image that is listed in two spellings once", func() {
		conf.Installation.RAW.PreloadImages.Images = []string{"rancher/hardened-calico:v3.32.2", agentImage}

		Expect(manager.preloadContainerImages(context.Background(), conf, rm, output)).To(Succeed())
		Expect(fetched).To(HaveLen(3))
	})

	It("preloads only the listed images when those of the release are excluded", func() {
		conf.Installation.RAW.PreloadImages = &install.PreloadImages{
			Release: new(false),
			Images:  []string{runtimeImage, agentImage},
		}

		Expect(manager.preloadContainerImages(context.Background(), conf, rm, output)).To(Succeed())
		Expect(fetched).To(Equal([]string{agentImage, runtimeImage}))
	})

	It("fails without the Kubernetes runtime image, which holds the container runtime", func() {
		conf.Installation.RAW.PreloadImages = &install.PreloadImages{
			Release: new(false),
			Images:  []string{agentImage},
		}

		err := manager.preloadContainerImages(context.Background(), conf, rm, output)
		Expect(err).To(MatchError(ContainSubstring("do not include the Kubernetes runtime image (rancher/rke2-runtime)")))
		Expect(fetched).To(BeEmpty())
	})

	It("fails when an image cannot be fetched", func() {
		manager.fetchImage = func(context.Context, string) (containerregistry.Image, error) {
			return nil, errors.New("no such image")
		}

		err := manager.preloadContainerImages(context.Background(), conf, rm, output)
		Expect(err).To(MatchError(ContainSubstring("fetching image 'docker.io/rancher/hardened-calico:v3.32.2': no such image")))
	})

	DescribeTable("names images as containerd does",
		func(ref, expected string) {
			name, _, err := containerdImageName(ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(name).To(Equal(expected))
		},
		Entry("Docker Hub, short", "rancher/shell:v0.8.3", "docker.io/rancher/shell:v0.8.3"),
		Entry("Docker Hub, official image", "busybox:1.36", "docker.io/library/busybox:1.36"),
		Entry("Docker Hub, full", "docker.io/rancher/shell:v0.8.3", "docker.io/rancher/shell:v0.8.3"),
		Entry("other registry", "quay.io/jetstack/cert-manager-webhook:v1.20.3", "quay.io/jetstack/cert-manager-webhook:v1.20.3"),
		Entry("registry with a port", "192.168.1.2:5000/e3/lcm:0.2.1", "192.168.1.2:5000/e3/lcm:0.2.1"),
		Entry("digest", "quay.io/a/b@sha256:"+digestOfZeros, "quay.io/a/b@sha256:"+digestOfZeros),
	)

	DescribeTable("names the runtime directory and its marker as RKE2 does",
		func(ref, registry, expectedDir, expectedMarker string) {
			dir, marker, err := rke2RuntimeDir(ref, registry)
			Expect(err).NotTo(HaveOccurred())
			Expect(dir).To(Equal(expectedDir))
			Expect(marker).To(Equal(expectedMarker))
		},
		Entry("as seen on a node of v1.37.1", runtimeImage, "",
			"v1.37.1-rke2r1-fce60b3fe2ba", "index.docker.io/rancher/rke2-runtime:v1.37.1-rke2r1"),
		Entry("as seen on a node of v1.37.2", "docker.io/rancher/rke2-runtime:v1.37.2-rke2r1", "",
			"v1.37.2-rke2r1-55cea3779caa", "index.docker.io/rancher/rke2-runtime:v1.37.2-rke2r1"),
		Entry("short reference", "rancher/rke2-runtime:v1.37.1-rke2r1", "",
			"v1.37.1-rke2r1-fce60b3fe2ba", "index.docker.io/rancher/rke2-runtime:v1.37.1-rke2r1"),
		Entry("with system-default-registry", "registry.example.com/rancher/rke2-runtime:v1.37.1-rke2r1", "registry.example.com",
			"v1.37.1-rke2r1-f6205f71364d", "registry.example.com/rancher/rke2-runtime:v1.37.1-rke2r1"),
	)

	It("needs a version tag on the runtime image", func() {
		_, _, err := rke2RuntimeDir("rancher/rke2-runtime@sha256:"+digestOfZeros, "")
		Expect(err).To(MatchError(ContainSubstring("has no version tag")))
	})
})

const digestOfZeros = "0000000000000000000000000000000000000000000000000000000000000000"
