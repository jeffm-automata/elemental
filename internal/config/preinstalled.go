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
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/suse/elemental/v3/internal/butane"
	"github.com/suse/elemental/v3/internal/image"
	"github.com/suse/elemental/v3/internal/image/kubernetes"
	"github.com/suse/elemental/v3/internal/template"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	growSystemUnitName   = "elemental-grow-system.service"
	growSystemScriptName = "grow-system.sh"
	// rke2ImportCacheName is the file in RKE2's image directory in which it records the archives
	// and image lists it has imported (k3s pkg/agent/containerd/watcher.go).
	rke2ImportCacheName = ".cache.json"
)

var (
	//go:embed templates/elemental-grow-system.service.tpl
	growSystemUnitTpl string

	//go:embed templates/grow-system.sh
	growSystemScript string
)

// configurePreinstalled adds what a preinstalled disk image needs at boot and installer media
// does not: the installer sizes the system partition to the disk it installs to, a preinstalled
// image was sized at build time.
func configurePreinstalled(butaneCfg *butane.Config) error {
	script := filepath.Join("/", image.ElementalPath(), growSystemScriptName)

	values := struct {
		Script string
	}{
		Script: script,
	}

	unit, err := template.Parse(growSystemUnitName, growSystemUnitTpl, &values)
	if err != nil {
		return fmt.Errorf("parsing %s unit template: %w", growSystemUnitName, err)
	}

	butaneCfg.AddFileInline(script, &growSystemScript, 0o744)
	butaneCfg.AddSystemdUnit(growSystemUnitName, unit, true)
	return nil
}

// preloadKubernetesImages places the image archives of the configuration directory in the
// directory RKE2 imports image archives from every time it starts, so that nodes do not pull
// these images from a registry.
func (m *Manager) preloadKubernetesImages(k *kubernetes.Kubernetes, output Output) error {
	if len(k.LocalImages) == 0 {
		return nil
	}

	fs := m.system.FS()
	target := filepath.Join(output.OverlaysDir(), image.RKE2AgentImagesPath())
	if err := vfs.MkdirAll(fs, target, vfs.DirPerm); err != nil {
		return fmt.Errorf("creating image archives directory: %w", err)
	}

	for _, archive := range k.LocalImages {
		m.system.Logger().Info("Preloading image archive %s", filepath.Base(archive))
		placed := filepath.Join(target, filepath.Base(archive))
		if err := vfs.CopyFile(fs, archive, placed); err != nil {
			return fmt.Errorf("copying image archive '%s': %w", archive, err)
		}
		// The record keeps modification times by the second and imports again whatever is newer
		// than its record: a file with a fraction of a second in its time is imported at every start.
		if raw, err := fs.RawPath(placed); err == nil {
			whole := time.Now().Truncate(time.Second)
			if err := os.Chtimes(raw, whole, whole); err != nil {
				return fmt.Errorf("setting the modification time of '%s': %w", placed, err)
			}
		}
	}

	// RKE2 imports every archive of this directory again each time it starts, unless the directory
	// holds the file it records imported archives in (size and modification time). It never creates
	// that file itself: an empty one switches the record on. Without it the archives of the image
	// are imported again after every reboot and every upgrade, for as long as they exist.
	cache := filepath.Join(target, rke2ImportCacheName)
	if exists, _ := vfs.Exists(fs, cache); !exists {
		if err := fs.WriteFile(cache, []byte{}, vfs.FilePerm); err != nil {
			return fmt.Errorf("creating the image import record '%s': %w", cache, err)
		}
	}

	return nil
}
