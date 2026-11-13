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

package deployment

import (
	"fmt"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/suse/elemental/v3/pkg/sys"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	// ReleaseMarkerFile is the path of the release marker of an installed system.
	ReleaseMarkerFile = "/etc/elemental/release.yaml"

	releaseMarkerHeader = "# self-generated content, do not edit\n\n"
)

// ReleaseMarker records which release an image was customized from.
//
// A system installed from installer media only knows that its OS came from the
// squashfs image of that media, so its deployment file cannot tell which OS image
// it runs. The tool that customizes the media knows the release manifest and the OS
// image this manifest pairs with the installer, and records them in this marker.
//
// The marker describes the first snapshot only. It is stored separately from the
// deployment file, which is parsed strictly, so that installers that do not know
// about it are not affected. Readers must ignore fields they do not know.
type ReleaseMarker struct {
	// ReleaseManifest is the URI of the release manifest the image was customized from.
	ReleaseManifest string `yaml:"releaseManifest"`
	// OSImage is the OS image of the release, exactly as written in the
	// 'components.operatingSystem.image.base' field of its core platform manifest.
	OSImage string `yaml:"osImage"`
	// InstallerImage is the image of the installer media the image was built from, exactly as
	// written in the 'components.operatingSystem.image.iso' field of the core platform manifest.
	InstallerImage string `yaml:"installerImage,omitempty"`
}

// Write stores the release marker under the given root.
func (m ReleaseMarker) Write(s *sys.System, root string) error {
	path := filepath.Join(root, ReleaseMarkerFile)
	err := vfs.MkdirAll(s.FS(), filepath.Dir(path), vfs.DirPerm)
	if err != nil {
		return fmt.Errorf("creating elemental directory: %w", err)
	}

	// The marker is read-only, a previous one has to go before it can be replaced.
	err = RemoveReleaseMarker(s, root)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshalling release marker: %w", err)
	}

	err = s.FS().WriteFile(path, append([]byte(releaseMarkerHeader), data...), 0444)
	if err != nil {
		return fmt.Errorf("writing release marker '%s': %w", path, err)
	}
	return nil
}

// ParseReleaseMarker reads the release marker from the given root. It returns
// nil if there is none. Unknown fields are ignored.
func ParseReleaseMarker(s *sys.System, root string) (*ReleaseMarker, error) {
	path := filepath.Join(root, ReleaseMarkerFile)
	if ok, _ := vfs.Exists(s.FS(), path); !ok {
		return nil, nil
	}

	data, err := s.FS().ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading release marker '%s': %w", path, err)
	}

	m := &ReleaseMarker{}
	if err = yaml.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("unmarshalling release marker '%s': %w", path, err)
	}
	return m, nil
}

// RemoveReleaseMarker deletes the release marker under the given root, if there is one.
func RemoveReleaseMarker(s *sys.System, root string) error {
	path := filepath.Join(root, ReleaseMarkerFile)
	if ok, _ := vfs.Exists(s.FS(), path); !ok {
		return nil
	}

	err := s.FS().Remove(path)
	if err != nil {
		return fmt.Errorf("removing release marker '%s': %w", path, err)
	}
	return nil
}
