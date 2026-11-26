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

package selinux

import (
	"bytes"
	"container/ring"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/suse/elemental/v3/pkg/chroot"
	"github.com/suse/elemental/v3/pkg/sys"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	SelinuxTargetedContextFile = selinuxTargetedPath + "/contexts/files/file_contexts"
	SelinuxTargetedPolicyType  = "targeted"

	defaultStoreRoot    = "/var/lib/selinux"
	selinuxTargetedPath = "/etc/selinux/" + SelinuxTargetedPolicyType
	selinuxAutoRelabel  = "/etc/selinux/.autorelabel"
	snapshotsDirName    = ".snapshots"
	selinuxRelabelled   = "/etc/selinux/.relabelled"
	selinuxConfig       = "/etc/selinux/config"
	selinuxContextsDir  = selinuxTargetedPath + "/contexts/files"
	debugLines          = 10
)

type relabelOpts struct {
	noAutoRelabel bool
}

// RelabelOpt changes what SystemRelabel does.
type RelabelOpt func(*relabelOpts)

// WithoutAutoRelabel makes SystemRelabel not ask for a relabel of the shared RW paths at the next boot, and withdraw
// such a request if the tree has one. It is for trees whose file contexts are the ones the shared paths are labelled
// with already, see SharedLabelsStillValid.
func WithoutAutoRelabel() RelabelOpt {
	return func(o *relabelOpts) { o.noAutoRelabel = true }
}

// SharedLabelsStillValid tells whether the shared RW paths of a running system, which were labelled by the policy in
// oldRoot, need no relabelling for the policy in newRoot. That is the case if
//   - a relabel at boot has completed on the old system and none is pending there, and
//   - the SE Linux configuration and every file context definition of the targeted policy are the same in both
//     trees (the compiled .bin companions are not compared: they are derived from the definitions).
//
// If so the labels the new policy would assign are the ones the old policy assigned. Anything that cannot be read or
// is missing counts as a difference; the reason is returned for the log.
func SharedLabelsStillValid(s *sys.System, oldRoot, newRoot string) (bool, string) {
	if ok, _ := vfs.Exists(s.FS(), filepath.Join(oldRoot, selinuxAutoRelabel)); ok {
		return false, "a relabel is pending on the running system"
	}
	if ok, _ := vfs.Exists(s.FS(), filepath.Join(oldRoot, selinuxRelabelled)); !ok {
		return false, "no completed relabel is recorded on the running system"
	}

	names := map[string]bool{}
	for _, root := range []string{oldRoot, newRoot} {
		entries, err := s.FS().ReadDir(filepath.Join(root, selinuxContextsDir))
		if err != nil {
			return false, fmt.Sprintf("reading file contexts: %s", err.Error())
		}
		for _, entry := range entries {
			if entry.Type().IsRegular() && !strings.HasSuffix(entry.Name(), ".bin") {
				names[filepath.Join(selinuxContextsDir, entry.Name())] = true
			}
		}
	}
	if !names[SelinuxTargetedContextFile] {
		return false, "no file contexts found"
	}
	names[selinuxConfig] = true

	for _, name := range slices.Sorted(maps.Keys(names)) {
		oldData, err := s.FS().ReadFile(filepath.Join(oldRoot, name))
		if err != nil {
			return false, fmt.Sprintf("'%s' of the running system: %s", name, err.Error())
		}
		newData, err := s.FS().ReadFile(filepath.Join(newRoot, name))
		if err != nil {
			return false, fmt.Sprintf("'%s' of the new system: %s", name, err.Error())
		}
		if !bytes.Equal(oldData, newData) {
			return false, fmt.Sprintf("'%s' changed", name)
		}
	}
	return true, ""
}

// SystemRelabel applies the SE Linux labels based on the targeted policy found within the given
// root path. It force applies the labels under the given root except for the given shared RW paths.
// This is to prevent runtime changes during the upgrades as RW paths are potentially in use for current
// processes. For snapshotted RW paths it applies SE Linux labels without force flag as it might include
// customized content merged with stock OS content.
// If at least one shared RW path is provided it also sets the .autorelabel file to trigger
// relabelling at boot and relabel the excluded paths, unless WithoutAutoRelabel is given.
func SystemRelabel(ctx context.Context, s *sys.System, rootDir string, snapshotted []string, shared []string, opts ...RelabelOpt) error {
	o := &relabelOpts{}
	for _, opt := range opts {
		opt(o)
	}
	contextFile := filepath.Join(rootDir, SelinuxTargetedContextFile)
	contextExists, _ := vfs.Exists(s.FS(), contextFile)

	if contextExists {
		var err error

		baseArgs := []string{"-i"}

		// We only keep last 10 lines of the stdout and stderr for debugging purposes
		stdOut := ring.New(debugLines)
		stdErr := ring.New(debugLines)

		if rootDir == "/" || rootDir == "" {
			rootDir = "/"
		} else {
			baseArgs = append(baseArgs, "-r", rootDir)
		}

		args := []string{"-F"}
		if len(snapshotted) > 0 {
			for _, path := range snapshotted {
				args = append(args, "-e", path)
			}
		}
		if len(shared) > 0 {
			for _, path := range shared {
				args = append(args, "-e", path)
			}
			autoRelabel := filepath.Join(rootDir, selinuxAutoRelabel)
			if o.noAutoRelabel {
				s.Logger().Info("Not requesting a relabel of shared RW paths at boot")
				err = s.FS().Remove(autoRelabel)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("removing .autorelabel file: %w", err)
				}
			} else {
				err = s.FS().WriteFile(autoRelabel, []byte{}, vfs.FilePerm)
				if err != nil {
					return fmt.Errorf("creating .autorelabel file: %w", err)
				}
			}
		}
		args = append(args, contextFile, rootDir)

		s.Logger().Info("Applying SE Linux labels to the read-only root tree, forced relabelling")
		err = s.Runner().RunContextParseOutput(ctx, stdHander(stdOut), stdHander(stdErr), "setfiles", slices.Concat(baseArgs, args)...)
		logOutput(s, stdOut, stdErr)

		if len(snapshotted) > 0 {
			s.Logger().Info("Applying SE Linux labels to snapshotted RW volumes")
			for _, path := range snapshotted {
				stdOut = ring.New(debugLines)
				stdErr = ring.New(debugLines)
				paths := []string{path}
				// The snapshots of a snapshotted RW volume are kept in a nested subvolume. setfiles does not
				// cross into another subvolume, and nothing mounts this one, so a relabel at boot does not
				// reach it either: it has to be named. The snapshots inside it are subvolumes again and are
				// left alone.
				snapshotsDir := filepath.Join(path, snapshotsDirName)
				if ok, _ := vfs.IsDir(s.FS(), snapshotsDir); ok {
					paths = append(paths, snapshotsDir)
				}
				err = s.Runner().RunContextParseOutput(ctx, stdHander(stdOut), stdHander(stdErr), "setfiles", slices.Concat(baseArgs, []string{contextFile}, paths)...)
				logOutput(s, stdOut, stdErr)
			}
		}

		return err
	}

	s.Logger().Warn("Not relabelling SE Linux, no context found")
	return nil
}

// ChrootedSystemRelabel applies the SE Linux labels based on the targeted policy found within the given
// root path. Runs the same logic as RelabelSystem method but running inside a chroot environment.
func ChrootedSystemRelabel(ctx context.Context, s *sys.System, rootDir string, snapshotted []string, shared []string, opts ...RelabelOpt) error {
	callback := func() error { return SystemRelabel(ctx, s, "/", snapshotted, shared, opts...) }
	err := chroot.ChrootedCallback(s, rootDir, nil, callback, chroot.WithoutDefaultBinds())
	if err != nil {
		return fmt.Errorf("chrooted system relabel: %w", err)
	}
	return nil
}

// RefreshPolicy recompiles the policy if its modules changed and otherwise regenerates
// the installed policy from the linked policy and local customizations.
func RefreshPolicy(ctx context.Context, s *sys.System, policyType string) error {
	const cmd = "semodule"
	args := []string{"-n", "--refresh", "-s", policyType}

	stdOut := ring.New(debugLines)
	stdErr := ring.New(debugLines)

	s.Logger().Info("Refreshing SE Linux policy from store '%s'", policyType)
	err := s.Runner().RunContextParseOutput(ctx, stdHander(stdOut), stdHander(stdErr), cmd, args...)
	logOutput(s, stdOut, stdErr)

	return err
}

// ChrootedRefreshPolicy runs RefreshPolicy for the specified policy in a chroot of the given
// root path. The refresh is skipped if the store root is the default policy store
// (/var/lib/selinux), or the custom store root has no active policy store.
func ChrootedRefreshPolicy(ctx context.Context, s *sys.System, rootDir, policyType string) error {
	callback := func() error {
		storeRoot, err := getStoreRoot(s)
		if err != nil {
			return fmt.Errorf("parsing store-root from policy configuration: %w", err)
		}

		// Do not refresh when the store is under /var, as it still holds old base modules and
		// rebuilding it would compile them into the new snapshot.
		if storeRoot == defaultStoreRoot {
			s.Logger().Info("SE Linux policy store is in the default %q, skipping policy refresh", storeRoot)
			return nil
		}

		// Refresh only on active policy store entry.
		store := filepath.Join(storeRoot, policyType, "active")
		if exists, _ := vfs.Exists(s.FS(), store); !exists {
			s.Logger().Info("No SE Linux policy store found at %q, skipping policy refresh", store)
			return nil
		}

		return RefreshPolicy(ctx, s, policyType)
	}
	err := chroot.ChrootedCallback(s, rootDir, nil, callback, chroot.WithoutDefaultBinds())
	if err != nil {
		return fmt.Errorf("chrooted policy refresh: %w", err)
	}
	return nil
}

// getStoreRoot parses the 'semanage.conf' file and returns the value of the
// 'store-root' property. In the event of a missing 'store-root' returns the default
// policy store - /var/lib/selinux.
func getStoreRoot(s *sys.System) (string, error) {
	const (
		semanageConf = "/etc/selinux/semanage.conf"
	)

	policyStoreRoot := defaultStoreRoot
	data, err := s.FS().ReadFile(semanageConf)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return policyStoreRoot, nil
		}
		return "", fmt.Errorf("reading policy store configuration: %w", err)
	}

	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "store-root" {
			continue
		}
		policyStoreRoot = filepath.Clean(strings.Trim(strings.TrimSpace(value), `"`))
	}

	return policyStoreRoot, nil
}

func stdHander(r *ring.Ring) func(string) {
	return func(line string) {
		r.Value = line
		r = r.Next()
	}
}

func logOutput(s *sys.System, stdOut, stdErr *ring.Ring) {
	output := "\n------- stdOut -------\n"
	stdOut.Do(func(v any) {
		if v != nil {
			output += v.(string) + "\n"
		}
	})
	output += "------- stdErr -------\n"
	stdErr.Do(func(v any) {
		if v != nil {
			output += v.(string) + "\n"
		}
	})
	output += "----------------------\n"
	s.Logger().Debug("SE Linux command output: %s", output)
}
