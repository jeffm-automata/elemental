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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	containerregistry "github.com/google/go-containerregistry/pkg/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/suse/elemental/v3/internal/image"
	"github.com/suse/elemental/v3/internal/image/kubernetes"
	"github.com/suse/elemental/v3/pkg/manifest/resolver"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	// rke2RuntimeRepository is the image RKE2 takes its container runtime (containerd, kubelet,
	// runc) and its packaged charts from when it starts.
	rke2RuntimeRepository = "rancher/rke2-runtime"
	// rke2DefaultRegistryKey is the RKE2 setting that prefixes the images RKE2 itself asks for.
	rke2DefaultRegistryKey = "system-default-registry"
	// containerdImageNameAnnotation is how an OCI archive names an image for containerd.
	containerdImageNameAnnotation = "io.containerd.image.name"

	preloadRuntimeDirs = "bin charts"
	// rke2ExtractedMarker is the file RKE2 leaves next to the extracted directories of its runtime image.
	rke2ExtractedMarker = ".extracted"
)

type imageFetchFunc func(ctx context.Context, imageRef string) (containerregistry.Image, error)

func (o Output) PreloadStoreDir() string {
	return filepath.Join(o.ExtractedFilesStoreDir(), "preload")
}

// PreloadRuntimeArchive is the tar archive of the Kubernetes runtime, as RKE2 extracts it on a node.
func (o Output) PreloadRuntimeArchive() string {
	return filepath.Join(o.PreloadStoreDir(), "runtime.tar")
}

// PreloadImagesArchive is the OCI archive of the container images to preload.
func (o Output) PreloadImagesArchive() string {
	return filepath.Join(o.PreloadStoreDir(), "images.tar")
}

// preloadContainerImages prepares what the build-time installation of a preinstalled image needs
// to fill the container image store of the installed system: one OCI archive with every image
// to preload, and the files of the Kubernetes runtime image, which hold the container runtime
// that imports the archive. Neither is part of the image; both are attached to the virtual
// machine that runs the installation.
func (m *Manager) preloadContainerImages(ctx context.Context, conf *image.Configuration, rm *resolver.ResolvedManifest, output Output) error {
	preload := conf.Installation.RAW.PreloadImages
	if preload == nil {
		return nil
	}

	logger := m.system.Logger()
	fs := m.system.FS()

	images := map[string]struct{}{}
	if preload.IncludeRelease() {
		release, err := m.collectContainerImages(rm, conf, output)
		if err != nil {
			return fmt.Errorf("collecting the container images of the release: %w", err)
		}
		for _, img := range release {
			images[img] = struct{}{}
		}
	}
	for _, img := range preload.Images {
		images[img] = struct{}{}
	}

	var runtime string
	names := map[string]string{}
	for img := range images {
		imgName, repository, err := containerdImageName(img)
		if err != nil {
			return fmt.Errorf("parsing image reference '%s': %w", img, err)
		}
		if other, ok := names[imgName]; ok {
			// The same image written in two ways, for example with and without docker.io.
			delete(images, max(img, other))
			img = min(img, other)
		}
		names[imgName] = img
		if strings.HasSuffix(repository, rke2RuntimeRepository) {
			runtime = img
		}
	}

	if runtime == "" {
		return fmt.Errorf("the images to preload do not include the Kubernetes runtime image (%s), "+
			"which holds the container runtime the image store is filled with", rke2RuntimeRepository)
	}

	if err := vfs.MkdirAll(fs, output.PreloadStoreDir(), vfs.DirPerm); err != nil {
		return fmt.Errorf("creating preload directory: %w", err)
	}

	serverConfig, err := kubernetes.ParseKubernetesConfig(m.system, conf.Kubernetes.Config.ServerFilePath)
	if err != nil {
		return fmt.Errorf("parsing server config: %w", err)
	}
	defaultRegistry, _ := serverConfig[rke2DefaultRegistryKey].(string)

	dataDir, marker, err := rke2RuntimeDir(runtime, defaultRegistry)
	if err != nil {
		return err
	}

	logger.Info("Preloading the Kubernetes runtime of %s as %s", runtime, dataDir)
	if err = m.writeRuntimeArchive(ctx, runtime, dataDir, marker, output); err != nil {
		return fmt.Errorf("preparing the Kubernetes runtime: %w", err)
	}

	sorted := slices.Sorted(maps.Keys(names))
	logger.Info("Preloading %d container images", len(sorted))

	archive, err := fs.Create(output.PreloadImagesArchive())
	if err != nil {
		return fmt.Errorf("creating image archive: %w", err)
	}
	defer archive.Close()

	w := newImageArchiveWriter(archive)
	for _, imgName := range sorted {
		logger.Info("Preloading image %s", imgName)

		img, err := m.fetchImage(ctx, names[imgName])
		if err != nil {
			return fmt.Errorf("fetching image '%s': %w", names[imgName], err)
		}
		if err = w.add(imgName, img); err != nil {
			return fmt.Errorf("archiving image '%s': %w", names[imgName], err)
		}
	}

	if err = w.close(); err != nil {
		return fmt.Errorf("closing image archive: %w", err)
	}

	return archive.Close()
}

// containerdImageName returns the name containerd and the kubelet know the image by, which
// always includes the registry, and its repository.
func containerdImageName(imageRef string) (imgName, repository string, err error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", "", err
	}

	registry := ref.Context().RegistryStr()
	if registry == name.DefaultRegistry {
		registry = dockerHubHost
	}

	separator := ":"
	if _, ok := ref.(name.Digest); ok {
		separator = "@"
	}

	repository = ref.Context().RepositoryStr()
	return registry + "/" + repository + separator + ref.Identifier(), repository, nil
}

// rke2RuntimeDir returns the directory name, below /var/lib/rancher/rke2/data, in which RKE2
// looks for the extracted files of its runtime image before it pulls and extracts the image,
// and the content of the file by which RKE2 knows that the extraction is complete: the version
// tag with the start of the hash of the reference RKE2 asks for, and the full name of that
// reference (rke2 pkg/bootstrap). Without a match the preloaded files are not used and RKE2
// extracts the image as it does today.
func rke2RuntimeDir(runtimeRef, defaultRegistry string) (dir, marker string, err error) {
	ref, err := name.ParseReference(runtimeRef)
	if err != nil {
		return "", "", fmt.Errorf("parsing runtime image reference '%s': %w", runtimeRef, err)
	}

	tag, ok := ref.(name.Tag)
	if !ok {
		return "", "", fmt.Errorf("runtime image reference '%s' has no version tag", runtimeRef)
	}

	asked := rke2RuntimeRepository + ":" + tag.TagStr()
	if defaultRegistry != "" {
		asked = defaultRegistry + "/" + asked
	}

	askedRef, err := name.ParseReference(asked)
	if err != nil {
		return "", "", fmt.Errorf("parsing runtime image reference '%s': %w", asked, err)
	}

	sum := sha256.Sum256([]byte(asked))
	return tag.TagStr() + "-" + hex.EncodeToString(sum[:])[:12], askedRef.Name(), nil
}

// writeRuntimeArchive archives the directories RKE2 extracts from its runtime image, below the
// directory name RKE2 extracts them to, and the file RKE2 writes when the extraction is complete.
func (m *Manager) writeRuntimeArchive(ctx context.Context, runtime, dataDir, marker string, output Output) (err error) {
	fs := m.system.FS()

	tempDir, err := vfs.TempDir(fs, output.PreloadStoreDir(), "runtime-")
	if err != nil {
		return fmt.Errorf("creating runtime directory: %w", err)
	}
	defer func() { _ = vfs.ForceRemoveAll(fs, tempDir) }()

	if err = m.unpackImage(ctx, runtime, tempDir); err != nil {
		return fmt.Errorf("unpacking '%s': %w", runtime, err)
	}

	archive, err := fs.Create(output.PreloadRuntimeArchive())
	if err != nil {
		return err
	}
	defer archive.Close()

	root, err := fs.RawPath(tempDir)
	if err != nil {
		return err
	}

	tw := tar.NewWriter(archive)
	for dir := range strings.FieldsSeq(preloadRuntimeDirs) {
		if exists, _ := vfs.Exists(fs, filepath.Join(tempDir, dir)); !exists {
			return fmt.Errorf("image '%s' has no '%s' directory", runtime, dir)
		}
		if err = addTree(tw, root, dir, dataDir); err != nil {
			return fmt.Errorf("archiving '%s': %w", dir, err)
		}
	}

	header := &tar.Header{
		Name:     filepath.Join(dataDir, rke2ExtractedMarker),
		Mode:     0o644,
		Size:     int64(len(marker)),
		Typeflag: tar.TypeReg,
	}
	if err = tw.WriteHeader(header); err != nil {
		return err
	}
	if _, err = tw.Write([]byte(marker)); err != nil {
		return err
	}

	if err = tw.Close(); err != nil {
		return err
	}

	return archive.Close()
}

// addTree adds the directory tree root/dir to the archive as prefix/dir, owned by root.
func addTree(tw *tar.Writer, root, dir, prefix string) error {
	return filepath.WalkDir(filepath.Join(root, dir), func(path string, entry iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		var link string
		if info.Mode()&iofs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}

		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header.Name = filepath.Join(prefix, rel)
		header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""

		if err = tw.WriteHeader(header); err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tw, file)
		return err
	})
}

// imageArchiveWriter writes container images as one OCI image layout in a tar archive, which
// containerd imports in a single pass. Each image keeps the manifest its registry serves, and a
// blob shared by several images is stored once.
type imageArchiveWriter struct {
	tw    *tar.Writer
	blobs map[string]struct{}
	index imageArchiveIndex
}

// imageArchiveIndex is the index.json of an OCI image layout.
type imageArchiveIndex struct {
	SchemaVersion int                    `json:"schemaVersion"`
	MediaType     string                 `json:"mediaType"`
	Manifests     []imageArchiveManifest `json:"manifests"`
}

type imageArchiveManifest struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

func newImageArchiveWriter(w io.Writer) *imageArchiveWriter {
	return &imageArchiveWriter{
		tw:    tar.NewWriter(w),
		blobs: map[string]struct{}{},
		index: imageArchiveIndex{SchemaVersion: 2, MediaType: ocispec.MediaTypeImageIndex},
	}
}

func (a *imageArchiveWriter) add(imgName string, img containerregistry.Image) error {
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("reading layers: %w", err)
	}

	for _, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			return err
		}
		if _, ok := a.blobs[digest.String()]; ok {
			continue
		}

		size, err := layer.Size()
		if err != nil {
			return err
		}

		blob, err := layer.Compressed()
		if err != nil {
			return err
		}

		err = a.blob(digest, size, blob)
		_ = blob.Close()
		if err != nil {
			return fmt.Errorf("writing layer %s: %w", digest, err)
		}
	}

	config, err := img.RawConfigFile()
	if err != nil {
		return fmt.Errorf("reading image configuration: %w", err)
	}
	configDigest, err := img.ConfigName()
	if err != nil {
		return err
	}
	if err = a.bytes(configDigest, config); err != nil {
		return err
	}

	manifest, err := img.RawManifest()
	if err != nil {
		return fmt.Errorf("reading image manifest: %w", err)
	}
	manifestDigest, err := img.Digest()
	if err != nil {
		return err
	}
	if err = a.bytes(manifestDigest, manifest); err != nil {
		return err
	}

	mediaType, err := img.MediaType()
	if err != nil {
		return err
	}

	a.index.Manifests = append(a.index.Manifests, imageArchiveManifest{
		MediaType:   string(mediaType),
		Digest:      manifestDigest.String(),
		Size:        int64(len(manifest)),
		Annotations: map[string]string{containerdImageNameAnnotation: imgName},
	})

	return nil
}

func (a *imageArchiveWriter) bytes(digest containerregistry.Hash, data []byte) error {
	if _, ok := a.blobs[digest.String()]; ok {
		return nil
	}
	return a.blob(digest, int64(len(data)), strings.NewReader(string(data)))
}

func (a *imageArchiveWriter) blob(digest containerregistry.Hash, size int64, r io.Reader) error {
	header := &tar.Header{
		Name:     filepath.Join(ocispec.ImageBlobsDir, digest.Algorithm, digest.Hex),
		Mode:     0o444,
		Size:     size,
		Typeflag: tar.TypeReg,
	}
	if err := a.tw.WriteHeader(header); err != nil {
		return err
	}
	if _, err := io.Copy(a.tw, r); err != nil {
		return err
	}

	a.blobs[digest.String()] = struct{}{}
	return nil
}

func (a *imageArchiveWriter) file(fileName string, content any) error {
	data, err := json.Marshal(content)
	if err != nil {
		return err
	}

	header := &tar.Header{Name: fileName, Mode: 0o444, Size: int64(len(data)), Typeflag: tar.TypeReg}
	if err = a.tw.WriteHeader(header); err != nil {
		return err
	}

	_, err = a.tw.Write(data)
	return err
}

func (a *imageArchiveWriter) close() error {
	if err := a.file(ocispec.ImageIndexFile, a.index); err != nil {
		return err
	}
	if err := a.file(ocispec.ImageLayoutFile, ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion}); err != nil {
		return err
	}

	return a.tw.Close()
}
