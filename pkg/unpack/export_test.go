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

package unpack

import (
	"context"
	"io/fs"

	"github.com/suse/elemental/v3/pkg/sys"
)

type CloneFunc = cloneFunc

func ReflinkPrepare(ctx context.Context, s *sys.System, source, destination string, excludes []string, clone CloneFunc) (bool, int, int) {
	ok, stats := reflinkPrepare(ctx, s, source, destination, excludes, clone)
	return ok, stats.Cloned, stats.Dropped
}

func Ficlone(src, dst string) error { return ficlone(src, dst) }

func ParseOverlayLayers(mountInfo, mountPoint string) ([]string, error) {
	return parseOverlayLayers(mountInfo, mountPoint)
}

func LayerFile(layers []string, rel string, merged fs.FileInfo) (string, error) {
	return layerFile(layers, rel, merged)
}

func QuickCheckFlags() []string { return quickCheckFlags() }

func ReflinkSyncEnabled() bool { return reflinkSyncEnabled() }
