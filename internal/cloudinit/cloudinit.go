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

// Package cloudinit applies cloud-init style configuration on Elemental systems using yip
// (github.com/rancher/yip) as the executor.
package cloudinit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rancher/yip/pkg/schema"
	"go.yaml.in/yaml/v3"

	"github.com/suse/elemental/v3/pkg/sys"
	"github.com/suse/elemental/v3/pkg/sys/vfs"
)

const (
	// RunDir holds the configuration fetched from a datasource for the current boot. It is
	// volatile on purpose: user-data may include credentials.
	RunDir = "/run/elemental/cloud-init"
	// StateDir holds the persistent state.
	StateDir = "/var/lib/elemental/cloud-init"

	userDataConfigFile = "00_userdata.yaml"
	userDataScriptFile = "user-data-script"
	instanceIDFile     = "instance-id"
	secretPerm         = os.FileMode(0o600)
	scriptPerm         = os.FileMode(0o700)
	secretDirPerm      = os.FileMode(0o700)
)

// DefaultConfigPaths returns the locations yip documents are loaded from, in order. The
// persistent ones are those used by elemental-toolkit based systems.
func DefaultConfigPaths() []string {
	return []string{"/system/oem", "/oem", "/usr/local/cloud-config", filepath.Join(RunDir, "config")}
}

// Config are the settings of a cloud-init run.
type Config struct {
	// ConfigPaths are the files or directories yip documents are loaded from.
	ConfigPaths []string
	// RunDir, see the RunDir constant.
	RunDir string
	// StateDir, see the StateDir constant.
	StateDir string
	// DatasourceTimeout is how long to wait for a datasource device to show up.
	DatasourceTimeout time.Duration
	// Strict turns errors applying a stage into a failure. Otherwise they are only logged, so
	// one faulty setting does not prevent the remaining ones from being applied.
	Strict bool
}

func NewConfig() *Config {
	return &Config{
		ConfigPaths: DefaultConfigPaths(),
		RunDir:      RunDir,
		StateDir:    StateDir,
	}
}

type CloudInit struct {
	s      *sys.System
	cfg    *Config
	runner stageRunner
}

type Opt func(c *CloudInit)

// WithStageRunner sets the stage runner, for testing purposes.
func WithStageRunner(r stageRunner) Opt {
	return func(c *CloudInit) {
		c.runner = r
	}
}

func New(s *sys.System, cfg *Config, opts ...Opt) *CloudInit {
	c := &CloudInit{s: s, cfg: cfg}
	for _, o := range opts {
		o(c)
	}
	if c.runner == nil {
		c.runner = newYipRunner(s)
	}
	return c
}

func (c *CloudInit) userDataConfigDir() string {
	return filepath.Join(c.cfg.RunDir, "config")
}

// Fetch probes the datasources and, if the user-data found was not applied yet to this
// instance, stores it as a yip document for the stages run during the current boot.
// Not finding a datasource is not an error.
func (c *CloudInit) Fetch() error {
	logger := c.s.Logger()

	ud, err := FetchUserData(c.s, filepath.Join(c.cfg.RunDir, "mnt"), c.cfg.DatasourceTimeout)
	if errors.Is(err, ErrNoDatasource) {
		logger.Info("No cloud-init datasource found, only local configuration is applied")
		return nil
	} else if err != nil {
		return fmt.Errorf("fetching user-data: %w", err)
	}

	if ud.InstanceID == "" {
		ud.InstanceID = fmt.Sprintf("sha256-%x", sha256.Sum256(ud.Data))
		logger.Info("Datasource %s provides no instance id, using the user-data checksum instead", ud.Source)
	}

	applied, err := c.appliedInstanceID()
	if err != nil {
		return err
	}
	if applied == ud.InstanceID {
		logger.Info("User-data from %s was already applied to instance '%s', skipping it", ud.Source, ud.InstanceID)
		return nil
	}

	logger.Info("Processing user-data from %s for instance '%s' (%d bytes)", ud.Source, ud.InstanceID, len(ud.Data))

	yipCfg, err := c.translate(ud)
	if err != nil {
		return fmt.Errorf("processing user-data from %s: %w", ud.Source, err)
	}
	if yipCfg == nil {
		return nil
	}

	data, err := yaml.Marshal(yipCfg)
	if err != nil {
		return fmt.Errorf("serializing configuration generated from user-data: %w", err)
	}

	configFile := filepath.Join(c.userDataConfigDir(), userDataConfigFile)
	if err = c.writeSecret(configFile, data, secretPerm); err != nil {
		return err
	}
	if err = c.writeSecret(filepath.Join(c.cfg.RunDir, instanceIDFile), []byte(ud.InstanceID+"\n"), secretPerm); err != nil {
		return err
	}

	logger.Info("Configuration generated from user-data written to %s", configFile)
	return nil
}

// translate converts the user-data into a yip configuration. It returns nil if the user-data
// is in a format which is not to be handled.
func (c *CloudInit) translate(ud *UserData) (*schema.YipConfig, error) {
	logger := c.s.Logger()
	data := bytes.TrimSpace(ud.Data)

	var cfg *schema.YipConfig

	switch {
	case len(data) == 0:
		logger.Info("User-data is empty")
		cfg = &schema.YipConfig{Name: "datasource metadata", Stages: map[string][]schema.Stage{}}
	case IsCloudConfig(data):
		var warnings []string
		var err error

		cfg, warnings, err = TranslateCloudConfig(data)
		if err != nil {
			return nil, err
		}
		for _, w := range warnings {
			logger.Warn("cloud-config: %s", w)
		}
	case bytes.HasPrefix(data, []byte("#!")):
		script := filepath.Join(c.cfg.RunDir, userDataScriptFile)
		if err := c.writeSecret(script, ud.Data, scriptPerm); err != nil {
			return nil, err
		}
		logger.Info("User-data is a script, it is run in the '%s' stage", StageNetwork)
		cfg = &schema.YipConfig{Name: "user-data script", Stages: map[string][]schema.Stage{
			StageNetwork: {{Name: "user-data script", Commands: []string{shellQuote(script)}}},
		}}
	case bytes.HasPrefix(data, []byte("{")):
		// Ignition is in charge of it, and it also is in charge of the hostname
		logger.Info("User-data looks like an Ignition configuration, ignoring it")
		return nil, nil
	default:
		yipCfg := &schema.YipConfig{}
		if err := yaml.Unmarshal(data, yipCfg); err != nil || len(yipCfg.Stages) == 0 {
			logger.Warn("User-data is not in a supported format (cloud-config, yip document or script), ignoring it")
			return nil, nil
		}
		logger.Info("User-data is a yip document")
		cfg = yipCfg
	}

	// The hostname in the user-data takes precedence over the one in the instance metadata
	if ud.Hostname != "" && !definesHostname(cfg) {
		logger.Info("Using hostname '%s' from the datasource metadata", ud.Hostname)
		cfg.Stages[StageEarly] = append(
			[]schema.Stage{{Name: "datasource hostname", Hostname: escapeTemplate(ud.Hostname)}},
			cfg.Stages[StageEarly]...,
		)
	}

	if len(cfg.Stages) == 0 {
		logger.Info("Nothing to apply from user-data")
	}

	return cfg, nil
}

func definesHostname(cfg *schema.YipConfig) bool {
	for _, stages := range cfg.Stages {
		for _, stage := range stages {
			if stage.Hostname != "" {
				return true
			}
		}
	}
	return false
}

// RunStage runs the given yip stage, including its '.before' and '.after' variants, from all
// the existing configuration paths.
func (c *CloudInit) RunStage(stage string) error {
	logger := c.s.Logger()

	var sources []string
	for _, path := range c.cfg.ConfigPaths {
		if exists, _ := vfs.Exists(c.s.FS(), path, true); !exists {
			logger.Debug("Skipping non existing cloud-init configuration path %s", path)
			continue
		}
		sources = append(sources, path)
	}
	if len(sources) == 0 {
		logger.Info("No cloud-init configuration found, nothing to run for stage '%s'", stage)
		return nil
	}

	var errs error
	for _, st := range []string{stage + ".before", stage, stage + ".after"} {
		logger.Info("Running cloud-init stage '%s' from %s", st, strings.Join(sources, ", "))
		if err := c.runner.Run(st, sources...); err != nil {
			errs = errors.Join(errs, fmt.Errorf("running stage '%s': %w", st, err))
		}
	}

	if errs != nil && !c.cfg.Strict {
		logger.Error("Errors found running cloud-init stage '%s', continuing as strict mode is not set: %s", stage, errs.Error())
		return nil
	}
	return errs
}

// Finalize records the instance the user-data fetched in the current boot belongs to, so it is
// not applied again on the following boots.
func (c *CloudInit) Finalize() error {
	pending := filepath.Join(c.cfg.RunDir, instanceIDFile)

	id, err := c.s.FS().ReadFile(pending)
	if err != nil {
		if exists, _ := vfs.Exists(c.s.FS(), pending); !exists {
			c.s.Logger().Debug("No user-data was fetched in this boot, nothing to finalize")
			return nil
		}
		return fmt.Errorf("reading instance id file '%s': %w", pending, err)
	}

	if err = c.writeSecret(filepath.Join(c.cfg.StateDir, instanceIDFile), id, secretPerm); err != nil {
		return err
	}
	c.s.Logger().Info("User-data for instance '%s' marked as applied", strings.TrimSpace(string(id)))
	return nil
}

func (c *CloudInit) appliedInstanceID() (string, error) {
	file := filepath.Join(c.cfg.StateDir, instanceIDFile)
	if exists, _ := vfs.Exists(c.s.FS(), file); !exists {
		return "", nil
	}
	id, err := c.s.FS().ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading instance id file '%s': %w", file, err)
	}
	return strings.TrimSpace(string(id)), nil
}

func (c *CloudInit) writeSecret(file string, data []byte, perm os.FileMode) error {
	fs := c.s.FS()
	if err := vfs.MkdirAll(fs, filepath.Dir(file), secretDirPerm); err != nil {
		return fmt.Errorf("creating directory for '%s': %w", file, err)
	}
	// Remove it first: WriteFile does not change the permissions of an existing file
	if err := fs.RemoveAll(file); err != nil {
		return fmt.Errorf("removing stale file '%s': %w", file, err)
	}
	if err := fs.WriteFile(file, data, perm); err != nil {
		return fmt.Errorf("writing file '%s': %w", file, err)
	}
	return nil
}
