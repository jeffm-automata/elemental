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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/suse/elemental/v3/pkg/rsync"
	"github.com/suse/elemental/v3/pkg/sys"
)

const (
	// ReflinkSyncEnv disables the reflink synchronization of unpacked containerd images when set to a false value.
	ReflinkSyncEnv = "ELEMENTAL_REFLINK_SYNC"

	mountInfoFile = "/proc/self/mountinfo"
)

// cloneFunc replaces the whole content of the file dst by the content of the file src without reading it.
type cloneFunc func(src, dst string) error

// reflinkStats counts what a reflink preparation did with the regular files that exist on both sides.
type reflinkStats struct {
	// Cloned files got the content of the source file by a reflink clone.
	Cloned int
	// Dropped files could not be cloned and were removed from the destination, so that rsync copies them.
	Dropped int
}

// reflinkSyncEnabled reports whether the reflink synchronization is enabled; it is unless the environment
// says otherwise.
func reflinkSyncEnabled() bool {
	v, ok := os.LookupEnv(ReflinkSyncEnv)
	if !ok || v == "" {
		return true
	}
	enabled, err := strconv.ParseBool(v)
	return err != nil || enabled
}

// quickCheckFlags returns the default rsync flags without the content comparison.
func quickCheckFlags() []string {
	return slices.DeleteFunc(rsync.DefaultFlags(), func(f string) bool { return f == "--checksum" })
}

// ficlone is the cloneFunc of file systems with reflink support (FICLONE). The destination is emptied first, as a
// clone never shortens a file.
func ficlone(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() {
		e := out.Close()
		if err == nil {
			err = e
		}
	}()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	return unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
}

// reflinkPrepare makes rsync's quick check (size and modification time) safe for a synchronization of source to
// destination, without reading file content. Image layers are commonly built with a constant modification time, so
// that an unchanged size and time say nothing about the content; this is why the default synchronization compares
// checksums, which reads every file on both sides.
//
// For every regular file of source that is a regular file in destination too, the content of the destination file is
// replaced by a reflink clone of the source file and its modification time is set to the one of the source. A file
// that cannot be cloned is removed from destination. After that no file of destination is merely assumed to be
// equal: each one either shares its data with the source or will be copied by rsync. Everything else (new files,
// deletions, file types, ownership, permissions, ACLs and extended attributes) is left to rsync.
//
// It returns false, and nothing that a checksum synchronization would not repair, if the very first clone fails
// (source and destination are not on the same reflink capable file system) or if anything unexpected happens. The
// caller must synchronize with checksums then.
func reflinkPrepare(ctx context.Context, s *sys.System, source, destination string, excludes []string, clone cloneFunc) (bool, reflinkStats) {
	var stats reflinkStats
	log := s.Logger()

	if raw, err := s.FS().RawPath(source); err == nil {
		source = raw
	}
	if raw, err := s.FS().RawPath(destination); err == nil {
		destination = raw
	}

	rooted := make([]string, 0, len(excludes))
	for _, e := range excludes {
		if strings.ContainsAny(e, "*?[") {
			log.Debug("reflink sync: exclude pattern '%s' is not a plain path", e)
			return false, stats
		}
		rooted = append(rooted, filepath.Clean(filepath.Join("/", e)))
	}
	excluded := func(rel string) bool {
		return slices.ContainsFunc(rooted, func(e string) bool { return rel == e || strings.HasPrefix(rel, e+"/") })
	}

	layers, err := overlayLayers(source)
	if err != nil {
		log.Debug("reflink sync: %s", err.Error())
		return false, stats
	}

	unsupported := false
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		rel = filepath.Join("/", rel)
		if excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		dst := filepath.Join(destination, rel)
		dstInfo, err := os.Lstat(dst)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		if err != nil {
			return err
		}
		if !dstInfo.Mode().IsRegular() {
			return nil
		}

		srcInfo, err := d.Info()
		if err != nil {
			return err
		}

		cerr := cloneRegular(path, rel, srcInfo, dst, dstInfo, layers, clone)
		if cerr == nil {
			stats.Cloned++
			return nil
		}

		// Not cloned: the destination file must not survive with its old content.
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing '%s' after a failed clone (%w): %w", dst, cerr, err)
		}
		if stats.Cloned == 0 && !errors.Is(cerr, errNotCloneable) {
			log.Debug("reflink sync: cannot clone '%s': %s", rel, cerr.Error())
			unsupported = true
			return filepath.SkipAll
		}
		log.Debug("reflink sync: '%s' not cloned, rsync will copy it: %s", rel, cerr.Error())
		stats.Dropped++
		return nil
	})
	if err != nil {
		log.Warn("reflink sync failed, synchronizing with checksums: %s", err.Error())
		return false, stats
	}
	if unsupported {
		return false, stats
	}
	return true, stats
}

// errNotCloneable marks a file that is not cloned by design; it says nothing about the file system.
var errNotCloneable = errors.New("file is not cloned")

// cloneRegular clones the regular file at src (relative path rel in the source tree) over the regular file dst and
// gives dst the modification time of src.
func cloneRegular(src, rel string, srcInfo fs.FileInfo, dst string, dstInfo fs.FileInfo, layers []string, clone cloneFunc) error {
	// A clone changes every name of the destination inode, and the names of an inode of the old tree may be
	// different files in the new one.
	if st, ok := dstInfo.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Errorf("%w: destination has %d hard links", errNotCloneable, st.Nlink)
	}

	real := src
	if len(layers) > 0 {
		var err error
		real, err = layerFile(layers, rel, srcInfo)
		if err != nil {
			return err
		}
	}

	if err := clone(real, dst); err != nil {
		return err
	}

	mtime := srcInfo.ModTime()
	ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, unix.NsecToTimespec(mtime.UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, dst, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("setting modification time: %w", err)
	}

	after, err := os.Lstat(dst)
	if err != nil {
		return err
	}
	if after.Size() != srcInfo.Size() || !after.ModTime().Equal(mtime) {
		return fmt.Errorf("unexpected size or time after the clone: %d bytes, %s", after.Size(), after.ModTime().Format(time.RFC3339Nano))
	}
	return nil
}

// layerFile returns the file that provides the regular file rel of an overlay mount: the one of the topmost layer
// that has the path. A file cannot be cloned through an overlay mount, as source and destination are on different
// file systems then, while its layer directory is on the file system the layers are stored on. The inode number and
// the size must be the ones the overlay mount shows, or the file is not the one it shows.
func layerFile(layers []string, rel string, merged fs.FileInfo) (string, error) {
	mst, ok := merged.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("%w: no inode number for the overlay file", errNotCloneable)
	}
	for _, layer := range layers {
		candidate := filepath.Join(layer, rel)
		info, err := os.Lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("%w: %w", errNotCloneable, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || st.Ino != mst.Ino || info.Size() != merged.Size() {
			// Not errNotCloneable: if this is the first file, the layers cannot be used at all.
			return "", fmt.Errorf("'%s' is not the file the overlay shows", candidate)
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%w: no layer has the file", errNotCloneable)
}

// overlayLayers returns the layer directories, topmost first, if the given directory is the mount point of an
// overlay file system, and nil if it is anything else. It fails for an overlay mount whose layers it cannot name.
func overlayLayers(dir string) ([]string, error) {
	data, err := os.ReadFile(mountInfoFile)
	if err != nil {
		// No mount table: the directory is used as it is. If it is an overlay the first clone fails.
		return nil, nil
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	return parseOverlayLayers(string(data), resolved)
}

// parseOverlayLayers looks up mountPoint in the content of a mountinfo file, see proc_pid_mountinfo(5). The last
// mount of a mount point is the visible one.
func parseOverlayLayers(mountInfo, mountPoint string) ([]string, error) {
	var fsType, superOpts string
	for line := range strings.Lines(mountInfo) {
		pre, post, found := strings.Cut(strings.TrimSpace(line), " - ")
		if !found {
			continue
		}
		fields := strings.Fields(pre)
		tail := strings.Fields(post)
		if len(fields) < 5 || len(tail) < 3 || unescapeMountPath(fields[4]) != mountPoint {
			continue
		}
		fsType, superOpts = tail[0], tail[2]
	}
	if fsType != "overlay" {
		return nil, nil
	}
	for opt := range strings.SplitSeq(superOpts, ",") {
		dirs, ok := strings.CutPrefix(opt, "lowerdir=")
		if !ok {
			continue
		}
		if strings.Contains(dirs, `\`) {
			return nil, fmt.Errorf("overlay layers of '%s' have escaped names", mountPoint)
		}
		layers := strings.Split(dirs, ":")
		for _, layer := range layers {
			if !filepath.IsAbs(layer) {
				return nil, fmt.Errorf("overlay layer '%s' of '%s' is not an absolute path", layer, mountPoint)
			}
		}
		return layers, nil
	}
	return nil, fmt.Errorf("overlay mount '%s' names no layers", mountPoint)
}

// unescapeMountPath decodes the octal escapes of a mountinfo path (space, tab, newline and backslash).
func unescapeMountPath(p string) string {
	if !strings.Contains(p, `\`) {
		return p
	}
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(p)
}
