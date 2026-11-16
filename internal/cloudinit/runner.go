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
	"os/exec"

	"github.com/rancher/yip/pkg/executor"
	"github.com/rancher/yip/pkg/logger"
	"github.com/rancher/yip/pkg/plugins"
	gvfs "github.com/twpayne/go-vfs/v4"

	"github.com/suse/elemental/v3/pkg/log"
	"github.com/suse/elemental/v3/pkg/sys"
)

// The yip wiring in this file is adapted from elemental-toolkit
// (github.com/rancher/elemental-toolkit, pkg/cloudinit, Apache-2.0).

// stageRunner runs a single yip stage from the given sources (files or directories).
type stageRunner interface {
	Run(stage string, sources ...string) error
}

type yipRunner struct {
	s    *sys.System
	exec executor.Executor
}

// newYipRunner returns a yip executor with the plugin set supported on Elemental systems.
//
// Compared to the yip defaults, the 'datasource' plugin is left out because datasources are
// probed by FetchUserData, and the 'layout' plugin is left out because partitions are managed
// by elemental3ctl.
func newYipRunner(s *sys.System) *yipRunner {
	return &yipRunner{
		s: s,
		exec: executor.NewExecutor(
			executor.WithConditionals(
				plugins.NodeConditional,
				plugins.IfConditional,
			),
			executor.WithLogger(&yipLogger{l: s.Logger()}),
			executor.WithPlugins(
				// the plugin execution order is the order given here
				plugins.DNS,
				plugins.Download,
				plugins.Entities,
				plugins.EnsureDirectories,
				plugins.EnsureFiles,
				plugins.Commands,
				plugins.DeleteEntities,
				plugins.Hostname,
				plugins.Sysctl,
				plugins.User,
				plugins.SSH,
				plugins.Timesyncd,
				plugins.Systemctl,
				plugins.Environment,
				plugins.SystemdFirstboot,
			),
		),
	}
}

func (r *yipRunner) Run(stage string, sources ...string) error {
	// yip requires the complete go-vfs interface, which the supported implementations of the
	// Elemental filesystem interface (OS and test filesystems) do provide.
	fs, ok := r.s.FS().(gvfs.FS)
	if !ok {
		return fmt.Errorf("running stage '%s': filesystem of type %T is not supported by yip", stage, r.s.FS())
	}
	return r.exec.Run(stage, fs, &console{s: r.s}, sources...)
}

// console implements the yip plugins.Console interface on top of the system runner.
type console struct {
	s *sys.System
}

var _ plugins.Console = (*console)(nil)

func (c *console) Run(command string, opts ...func(cmd *exec.Cmd)) (string, error) {
	if len(opts) > 0 {
		// Only the yip plugins left out in newYipRunner pass command options
		return "", fmt.Errorf("running '%s': command options are not supported", command)
	}

	out, err := c.s.Runner().Run("sh", "-c", command)
	if err != nil {
		return string(out), fmt.Errorf("running '%s': %w", command, err)
	}
	return string(out), nil
}

func (c *console) Start(cmd *exec.Cmd, opts ...func(cmd *exec.Cmd)) error {
	for _, o := range opts {
		o(cmd)
	}
	c.s.Logger().Debug("Running cmd: '%s'", cmd.String())
	return cmd.Run()
}

func (c *console) RunTemplate(st []string, template string) error {
	var errs error
	for _, svc := range st {
		out, err := c.Run(fmt.Sprintf(template, svc))
		if err != nil {
			c.s.Logger().Error("%s: %s", err.Error(), out)
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// yipLogger adapts the Elemental logger to the yip logger interface.
type yipLogger struct {
	l log.Logger
}

var _ logger.Interface = (*yipLogger)(nil)

const strFmt = "%s"

func (y *yipLogger) Info(args ...any)  { y.l.Info(strFmt, fmt.Sprint(args...)) }
func (y *yipLogger) Warn(args ...any)  { y.l.Warn(strFmt, fmt.Sprint(args...)) }
func (y *yipLogger) Debug(args ...any) { y.l.Debug(strFmt, fmt.Sprint(args...)) }
func (y *yipLogger) Error(args ...any) { y.l.Error(strFmt, fmt.Sprint(args...)) }
func (y *yipLogger) Fatal(args ...any) { y.l.Fatal(strFmt, fmt.Sprint(args...)) }
func (y *yipLogger) Panic(args ...any) { y.l.Panic(strFmt, fmt.Sprint(args...)) }
func (y *yipLogger) Trace(args ...any) { y.l.Trace(strFmt, fmt.Sprint(args...)) }

func (y *yipLogger) Infof(f string, args ...any)  { y.l.Info(f, args...) }
func (y *yipLogger) Warnf(f string, args ...any)  { y.l.Warn(f, args...) }
func (y *yipLogger) Debugf(f string, args ...any) { y.l.Debug(f, args...) }
func (y *yipLogger) Errorf(f string, args ...any) { y.l.Error(f, args...) }
func (y *yipLogger) Fatalf(f string, args ...any) { y.l.Fatal(f, args...) }
func (y *yipLogger) Panicf(f string, args ...any) { y.l.Panic(f, args...) }
func (y *yipLogger) Tracef(f string, args ...any) { y.l.Trace(f, args...) }
