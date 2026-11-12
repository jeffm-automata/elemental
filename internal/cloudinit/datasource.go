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

package cloudinit

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/suse/elemental/v3/pkg/sys"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	labelDir = "/dev/disk/by-label"
)

// UserData is the outcome of probing the supported datasources.
type UserData struct {
	// Source describes where the data was found, for logging.
	Source string
	// InstanceID identifies the instance the user-data was meant for. Empty if the
	// datasource does not provide one.
	InstanceID string
	// Hostname is the hostname provided by the datasource metadata, if any.
	Hostname string
	// Data is the raw user-data.
	Data []byte
}

// ErrNoDatasource is returned if no supported datasource is attached to the machine.
var ErrNoDatasource = errors.New("no cloud-init datasource found")

// volumeSource describes a datasource provided as a labelled filesystem.
type volumeSource struct {
	name     string
	labels   []string
	userData string
	metaData string
	parse    func(metaData []byte, ud *UserData) error
}

// volumeSources lists the datasources of the first cut: the ones a hypervisor attaches as a
// block device, so they are readable before the network is configured.
//
// yip has a 'datasource' plugin (backed by github.com/rancher-sandbox/linuxkit providers) that
// reads the same volumes. It is not used because it only returns the user-data: the instance
// metadata (instance id and hostname) is ignored, and without it a machine created by KubeVirt
// or by the Cluster API provider for Harvester has no hostname.
var volumeSources = []volumeSource{
	{
		// https://cloudinit.readthedocs.io/en/latest/reference/datasources/nocloud.html
		name:     "NoCloud",
		labels:   []string{"cidata", "CIDATA"},
		userData: "user-data",
		metaData: "meta-data",
		parse:    parseNoCloudMetaData,
	}, {
		// https://cloudinit.readthedocs.io/en/latest/reference/datasources/configdrive.html
		name:     "ConfigDrive",
		labels:   []string{"config-2", "CONFIG-2"},
		userData: "openstack/latest/user_data",
		metaData: "openstack/latest/meta_data.json",
		parse:    parseConfigDriveMetaData,
	},
}

func parseNoCloudMetaData(metaData []byte, ud *UserData) error {
	md := struct {
		InstanceID    string `yaml:"instance-id"`
		LocalHostname string `yaml:"local-hostname"`
	}{}
	if err := yaml.Unmarshal(metaData, &md); err != nil {
		return err
	}
	ud.InstanceID = md.InstanceID
	ud.Hostname = md.LocalHostname
	return nil
}

func parseConfigDriveMetaData(metaData []byte, ud *UserData) error {
	// JSON is a subset of YAML
	md := struct {
		UUID     string `yaml:"uuid"`
		Hostname string `yaml:"hostname"`
	}{}
	if err := yaml.Unmarshal(metaData, &md); err != nil {
		return err
	}
	ud.InstanceID = md.UUID
	ud.Hostname = md.Hostname
	return nil
}

// FetchUserData probes the supported datasources and returns the user-data of the first one
// found. The lookup is retried until the given timeout expires, as the block device may show
// up late. It returns ErrNoDatasource if there is none.
func FetchUserData(s *sys.System, mountPoint string, timeout time.Duration) (*UserData, error) {
	deadline := time.Now().Add(timeout)

	for {
		for _, src := range volumeSources {
			for _, label := range src.labels {
				device := filepath.Join(labelDir, label)
				if exists, _ := vfs.Exists(s.FS(), device, true); !exists {
					continue
				}
				s.Logger().Info("Found %s datasource at device %s", src.name, device)
				return readVolume(s, src, device, mountPoint)
			}
		}
		if !time.Now().Before(deadline) {
			return nil, ErrNoDatasource
		}
		time.Sleep(time.Second)
	}
}

func readVolume(s *sys.System, src volumeSource, device, mountPoint string) (ud *UserData, err error) {
	if err = vfs.MkdirAll(s.FS(), mountPoint, vfs.DirPerm); err != nil {
		return nil, fmt.Errorf("creating mount point '%s' for %s datasource: %w", mountPoint, src.name, err)
	}

	// 'auto' as the volume is either ISO9660 or VFAT
	if err = s.Mounter().Mount(device, mountPoint, "auto", []string{"ro"}); err != nil {
		return nil, fmt.Errorf("mounting %s datasource device '%s' at '%s': %w", src.name, device, mountPoint, err)
	}
	defer func() {
		if uErr := s.Mounter().Unmount(mountPoint); uErr != nil {
			err = errors.Join(err, fmt.Errorf("unmounting %s datasource from '%s': %w", src.name, mountPoint, uErr))
		}
	}()

	ud = &UserData{Source: fmt.Sprintf("%s (%s)", src.name, device)}

	userDataFile := filepath.Join(mountPoint, src.userData)
	ud.Data, err = s.FS().ReadFile(userDataFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading user-data file '%s' of %s datasource: %w", src.userData, src.name, err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		s.Logger().Info("%s datasource has no '%s' file", src.name, src.userData)
	}

	metaDataFile := filepath.Join(mountPoint, src.metaData)
	metaData, err := s.FS().ReadFile(metaDataFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.Logger().Info("%s datasource has no '%s' file", src.name, src.metaData)
	case err != nil:
		return nil, fmt.Errorf("reading metadata file '%s' of %s datasource: %w", src.metaData, src.name, err)
	default:
		if err = src.parse(metaData, ud); err != nil {
			return nil, fmt.Errorf("parsing metadata file '%s' of %s datasource: %w", src.metaData, src.name, err)
		}
	}

	ud.Hostname = strings.TrimSpace(ud.Hostname)
	ud.InstanceID = strings.TrimSpace(ud.InstanceID)

	return ud, nil
}
