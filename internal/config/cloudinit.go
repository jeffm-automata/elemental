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
	"path/filepath"

	"github.com/suse/elemental/v3/internal/butane"
	"github.com/suse/elemental/v3/internal/image"
	"github.com/suse/elemental/v3/internal/template"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	cloudInitBinaryName      = "elemental3ctl"
	cloudInitEarlyUnitName   = "elemental-cloud-init-early.service"
	cloudInitBootUnitName    = "elemental-cloud-init-boot.service"
	cloudInitNetworkUnitName = "elemental-cloud-init-network.service"
)

var (
	//go:embed templates/elemental-cloud-init-early.service.tpl
	cloudInitEarlyUnitTpl string

	//go:embed templates/elemental-cloud-init-boot.service.tpl
	cloudInitBootUnitTpl string

	//go:embed templates/elemental-cloud-init-network.service.tpl
	cloudInitNetworkUnitTpl string
)

// configureCloudInit includes in the image the services applying cloud-init style
// configuration at boot time and the elemental3ctl binary they run.
//
// The binary is the one shipped along with the running tool: this guarantees the 'cloud-init'
// subcommand is available regardless of the elemental3ctl version of the OS image.
func (m *Manager) configureCloudInit(output Output, butaneCfg *butane.Config) error {
	fs := m.system.FS()

	if host := m.system.Platform().String(); m.platform != host {
		return fmt.Errorf("no %s binary available for platform %s on a %s host", cloudInitBinaryName, m.platform, host)
	}

	binary, err := m.cloudInitBinary()
	if err != nil {
		return fmt.Errorf("locating %s executable: %w", cloudInitBinaryName, err)
	}

	target := filepath.Join(output.OverlaysDir(), image.CloudInitBinaryPath())
	if err = vfs.MkdirAll(fs, filepath.Dir(target), vfs.DirPerm); err != nil {
		return fmt.Errorf("creating %s binary directory: %w", cloudInitBinaryName, err)
	}

	if err = vfs.CopyFile(fs, binary, target); err != nil {
		return fmt.Errorf("copying %s binary from '%s': %w", cloudInitBinaryName, binary, err)
	}

	if err = fs.Chmod(target, 0o755); err != nil {
		return fmt.Errorf("setting %s binary permissions: %w", cloudInitBinaryName, err)
	}

	values := struct {
		Binary      string
		EarlyUnit   string
		BootUnit    string
		K8sUnit     string
		NetworkUnit string
	}{
		Binary:      filepath.Join("/", image.CloudInitBinaryPath()),
		EarlyUnit:   cloudInitEarlyUnitName,
		BootUnit:    cloudInitBootUnitName,
		K8sUnit:     k8sConfigUnitName,
		NetworkUnit: cloudInitNetworkUnitName,
	}

	units := []struct {
		name string
		tpl  string
	}{
		{cloudInitEarlyUnitName, cloudInitEarlyUnitTpl},
		{cloudInitBootUnitName, cloudInitBootUnitTpl},
		{cloudInitNetworkUnitName, cloudInitNetworkUnitTpl},
	}

	for _, unit := range units {
		data, err := template.Parse(unit.name, unit.tpl, &values)
		if err != nil {
			return fmt.Errorf("parsing %s unit template: %w", unit.name, err)
		}
		butaneCfg.AddSystemdUnit(unit.name, data, true)
	}

	m.system.Logger().Info("cloud-init services included in the image")

	return nil
}
