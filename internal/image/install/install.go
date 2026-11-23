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

package install

import (
	"math/bits"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/docker/go-units"

	"github.com/suse/elemental/v3/pkg/crypto"
)

type DiskSize string

func (d DiskSize) IsValid() bool {
	return regexp.MustCompile(`^[1-9]\d*[KMGT]$`).MatchString(string(d))
}

// returns the size in MiB
func (d DiskSize) ToMiB() (uint64, error) {
	size := strings.TrimSpace(string(d))
	dimension := uint64(1)
	switch unicode.ToUpper(rune(size[len(size)-1])) {
	case 'K':
		dimension = units.KiB
	case 'M':
		dimension = units.MiB
	case 'G':
		dimension = units.GiB
	case 'T':
		dimension = units.TiB
	}

	value, err := strconv.ParseUint(size[:len(size)-1], 10, bits.UintSize)
	if err != nil {
		return 0, err
	}

	return value * dimension / units.MiB, nil
}

type Installation struct {
	SchemaVersion string        `yaml:"schema"`
	Bootloader    string        `yaml:"bootloader" validate:"omitempty,oneof=grub none"`
	KernelCmdLine string        `yaml:"kernelCmdLine"`
	RAW           RAW           `yaml:"raw"`
	ISO           ISO           `yaml:"iso"`
	CryptoPolicy  crypto.Policy `yaml:"cryptoPolicy" validate:"omitempty,oneof=fips default"`
	CloudInit     CloudInit     `yaml:"cloudInit,omitempty"`
}

// CloudInit configures the cloud-init style configuration of the system at boot time.
type CloudInit struct {
	// Enabled includes the cloud-init services in the image. They apply the user-data found in
	// a NoCloud or ConfigDrive volume and the yip documents stored in the system.
	Enabled bool `yaml:"enabled"`
}

type RAW struct {
	DiskSize DiskSize `yaml:"diskSize" validate:"omitempty,disksize"`
	// Preinstalled produces a disk image that already holds the installed system instead of
	// installer media: the installation the image would run on its first boot is run once at
	// build time, in a virtual machine. A machine created from the image boots the final
	// system directly; the system partition is grown to the size of the disk at boot.
	Preinstalled bool `yaml:"preinstalled,omitempty"`
	// PreloadImages fills the container image store of a preinstalled image at build time, so
	// that a machine created from the image starts its Kubernetes node without pulling and
	// unpacking these images. Requires Preinstalled.
	PreloadImages *PreloadImages `yaml:"preloadImages,omitempty"`
}

// PreloadImages lists the container images a preinstalled image holds, pulled and unpacked.
type PreloadImages struct {
	// Release includes the images the release itself needs on a node: the image sets of the
	// Kubernetes distribution for the configured CNI and ingress controller, and the images the
	// release manifest or the configuration directory declare for the enabled Helm charts and
	// manifests. Defaults to true.
	Release *bool `yaml:"release,omitempty"`
	// Images are more image references to include, for example what a management server makes
	// every node of an imported cluster pull.
	Images []string `yaml:"images,omitempty" validate:"dive,required"`
}

// IncludeRelease tells whether the images of the release are part of the preloaded images.
func (p *PreloadImages) IncludeRelease() bool {
	return p != nil && (p.Release == nil || *p.Release)
}

type ISO struct {
	Device     string `yaml:"device"`
	NetbootURL string `yaml:"netbootURL" validate:"omitempty,netbooturl"`
}
