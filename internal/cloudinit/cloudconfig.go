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
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rancher/yip/pkg/schema"
	"go.yaml.in/yaml/v3"
)

const (
	// StageEarly is the yip stage that carries the cloud-config settings which have to be in
	// place before the network and any other service is started: hostname, users, SSH keys
	// and written files. The name is the one elemental-toolkit uses for the same purpose.
	StageEarly = "initramfs"
	// StageFS is run right after StageEarly, kept for compatibility with elemental-toolkit
	// yip documents.
	StageFS = "fs"
	// StageBoot is the yip stage run once basic system initialization is complete.
	StageBoot = "boot"
	// StageNetwork is the yip stage run once the network is online. It carries the cloud-config
	// 'runcmd' commands.
	StageNetwork = "network"

	cloudConfigHeader = "#cloud-config"
	jinjaHeader       = "## template: jinja"
	defaultFilePerm   = 0o644
	rootUser          = "root"
	maxGunzipSize     = 64 << 20

	keyHostname = "hostname"
	keyUsers    = "users"
	keySSHKeys  = "ssh_authorized_keys"
	keyFiles    = "write_files"
	keyRunCmd   = "runcmd"
)

// supportedKeys lists the top level cloud-config keys this implementation applies.
// Any other key is reported and ignored.
var supportedKeys = []string{keyHostname, keyUsers, keySSHKeys, keyFiles, keyRunCmd}

type ccFile struct {
	Path        string    `yaml:"path"`
	Content     string    `yaml:"content"`
	Encoding    string    `yaml:"encoding"`
	Owner       string    `yaml:"owner"`
	Permissions yaml.Node `yaml:"permissions"`
	Append      bool      `yaml:"append"`
	Defer       bool      `yaml:"defer"`
}

type ccUser struct {
	Name              string    `yaml:"name"`
	PasswordHash      string    `yaml:"passwd"`
	LockPasswd        *bool     `yaml:"lock_passwd"`
	SSHAuthorizedKeys []string  `yaml:"ssh_authorized_keys"`
	GECOS             string    `yaml:"gecos"`
	Homedir           string    `yaml:"homedir"`
	NoCreateHome      bool      `yaml:"no_create_home"`
	PrimaryGroup      string    `yaml:"primary_group"`
	Groups            yaml.Node `yaml:"groups"`
	System            bool      `yaml:"system"`
	Shell             string    `yaml:"shell"`
	UID               string    `yaml:"uid"`
	Sudo              yaml.Node `yaml:"sudo"`
}

// IsCloudConfig tells whether the given user-data is a cloud-config document. The
// '#cloud-config' marker has to be within the leading comment lines; cloud-init allows
// other comment headers, such as '## template: jinja', to precede it.
func IsCloudConfig(data []byte) bool {
	_, found := leadingHeaders(data)[cloudConfigHeader]
	return found
}

func leadingHeaders(data []byte) map[string]struct{} {
	headers := map[string]struct{}{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		headers[line] = struct{}{}
	}
	return headers
}

// TranslateCloudConfig converts a cloud-config document into a yip configuration.
//
// yip ships its own cloud-config loader, it is not used here because:
//   - it fails on documents holding list form 'runcmd' items or a 'users' item which is not
//     a map, both valid for cloud-init and emitted by Cluster API providers
//   - it only detects a document starting exactly with '#cloud-config'
//   - it runs all settings but the hostname in a single stage, while 'runcmd' must be run
//     once the network is online and the rest before the network is configured
//   - it silently drops unknown keys
//
// The returned warnings describe, in detail, every setting which was found and not applied.
func TranslateCloudConfig(data []byte) (*schema.YipConfig, []string, error) {
	var warnings []string

	if !IsCloudConfig(data) {
		return nil, nil, fmt.Errorf("document is missing the '%s' header", cloudConfigHeader)
	}

	if _, jinja := leadingHeaders(data)[jinjaHeader]; jinja && bytes.Contains(data, []byte("{{")) {
		warnings = append(warnings, fmt.Sprintf(
			"document is declared as a jinja template ('%s'), jinja rendering is not supported and "+
				"any '{{ ... }}' expression is kept literally", jinjaHeader))
	}

	doc := map[string]yaml.Node{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("parsing cloud-config document: %w", err)
	}

	early := schema.Stage{
		Name:    "cloud-config early settings",
		Users:   map[string]schema.User{},
		SSHKeys: map[string][]string{},
	}
	network := schema.Stage{Name: "cloud-config runcmd"}

	for _, key := range sortedKeys(doc) {
		node := doc[key]
		var w []string
		var err error

		switch key {
		case keyHostname:
			err = node.Decode(&early.Hostname)
		case keyUsers:
			w, err = translateUsers(&node, &early)
		case keySSHKeys:
			var keys []string
			if err = node.Decode(&keys); err == nil {
				var valid []string
				valid, w = filterSSHKeys(rootUser, keys)
				early.SSHKeys[rootUser] = append(early.SSHKeys[rootUser], valid...)
			}
		case keyFiles:
			early.Files, w, err = translateFiles(&node)
		case keyRunCmd:
			network.Commands, err = translateCommands(&node)
		default:
			w = []string{fmt.Sprintf("ignoring unsupported cloud-config key '%s' (line %d), supported keys are: %s",
				key, node.Line, strings.Join(supportedKeys, ", "))}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("parsing cloud-config key '%s' (line %d): %w", key, node.Line, err)
		}
		warnings = append(warnings, w...)
	}

	cfg := &schema.YipConfig{Name: "cloud-config", Stages: map[string][]schema.Stage{}}
	if early.Hostname != "" || len(early.Users) > 0 || len(early.SSHKeys) > 0 || len(early.Files) > 0 {
		early.Hostname = escapeTemplate(early.Hostname)
		cfg.Stages[StageEarly] = []schema.Stage{early}
	}
	if len(network.Commands) > 0 {
		cfg.Stages[StageNetwork] = []schema.Stage{network}
	}

	return cfg, warnings, nil
}

func sortedKeys(m map[string]yaml.Node) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func translateUsers(node *yaml.Node, stage *schema.Stage) ([]string, error) {
	var warnings []string

	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected a list of users")
	}

	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			warnings = append(warnings, fmt.Sprintf(
				"ignoring 'users' item '%s' (line %d): only user definitions with a 'name' are supported, "+
					"there is no default user", item.Value, item.Line))
			continue
		}

		var u ccUser
		if err := item.Decode(&u); err != nil {
			return nil, fmt.Errorf("decoding user (line %d): %w", item.Line, err)
		}
		if u.Name == "" {
			return nil, fmt.Errorf("user (line %d) is missing the 'name' field", item.Line)
		}

		groups, err := stringOrList(&u.Groups)
		if err != nil {
			return nil, fmt.Errorf("decoding groups of user '%s': %w", u.Name, err)
		}
		if !u.Sudo.IsZero() {
			warnings = append(warnings, fmt.Sprintf(
				"ignoring unsupported 'sudo' setting of user '%s' (line %d)", u.Name, u.Sudo.Line))
		}

		keys, w := filterSSHKeys(u.Name, u.SSHAuthorizedKeys)
		warnings = append(warnings, w...)

		stage.Users[u.Name] = schema.User{
			Name:         u.Name,
			PasswordHash: u.PasswordHash,
			// cloud-init locks the password unless told otherwise
			LockPasswd:   u.LockPasswd == nil || *u.LockPasswd,
			GECOS:        u.GECOS,
			Homedir:      u.Homedir,
			NoCreateHome: u.NoCreateHome,
			PrimaryGroup: u.PrimaryGroup,
			Groups:       groups,
			System:       u.System,
			Shell:        u.Shell,
			UID:          u.UID,
		}
		if len(keys) > 0 {
			stage.SSHKeys[u.Name] = append(stage.SSHKeys[u.Name], keys...)
		}
	}

	return warnings, nil
}

// filterSSHKeys drops any key yip would not handle as a literal public key. yip fetches keys
// expressed as an URL or as 'github:<user>' from the network, cloud-config keys are literals.
func filterSSHKeys(user string, keys []string) (valid []string, warnings []string) {
	for _, key := range keys {
		key = strings.TrimSpace(key)
		fields := strings.Fields(key)
		if len(fields) < 2 || strings.Contains(fields[0], ":") {
			warnings = append(warnings, fmt.Sprintf(
				"ignoring SSH authorized key of user '%s': '%.40s' is not a public key line", user, key))
			continue
		}
		valid = append(valid, key)
	}
	return valid, warnings
}

func stringOrList(node *yaml.Node) ([]string, error) {
	var list []string

	switch node.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		for item := range strings.SplitSeq(node.Value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				list = append(list, item)
			}
		}
		return list, nil
	case yaml.SequenceNode:
		err := node.Decode(&list)
		return list, err
	default:
		return nil, fmt.Errorf("expected a string or a list of strings (line %d)", node.Line)
	}
}

func translateFiles(node *yaml.Node) ([]schema.File, []string, error) {
	var ccFiles []ccFile
	var files []schema.File
	var warnings []string

	if err := node.Decode(&ccFiles); err != nil {
		return nil, nil, err
	}

	for i, f := range ccFiles {
		if f.Path == "" {
			return nil, nil, fmt.Errorf("file number %d is missing the 'path' field", i+1)
		}
		if f.Append {
			warnings = append(warnings, fmt.Sprintf(
				"skipping file '%s': 'append: true' is not supported and the file would be overwritten", f.Path))
			continue
		}
		if f.Defer {
			warnings = append(warnings, fmt.Sprintf(
				"file '%s': 'defer: true' is not supported, the file is written before the network is configured", f.Path))
		}

		perm := uint32(defaultFilePerm)
		if !f.Permissions.IsZero() && f.Permissions.Value != "" {
			p, err := strconv.ParseUint(f.Permissions.Value, 8, 32)
			if err != nil || p > 0o7777 {
				return nil, nil, fmt.Errorf("file '%s' has invalid permissions '%s', expected an octal value such as '0644'",
					f.Path, f.Permissions.Value)
			}
			perm = uint32(p)
		}

		content, err := decodeContent(f.Content, f.Encoding)
		if err != nil {
			return nil, nil, fmt.Errorf("decoding content of file '%s' (encoding '%s'): %w", f.Path, f.Encoding, err)
		}

		files = append(files, schema.File{
			Path:        f.Path,
			Content:     escapeTemplate(content),
			Permissions: perm,
			OwnerString: f.Owner,
		})
	}

	return files, warnings, nil
}

func decodeContent(content, encoding string) (string, error) {
	b64 := []string{"b64", "base64"}
	gz := []string{"gz", "gzip"}
	gzB64 := []string{"gz+base64", "gzip+base64", "gz+b64", "gzip+b64"}

	data := []byte(content)
	var err error

	if slices.Contains(b64, encoding) || slices.Contains(gzB64, encoding) {
		data, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(content), ""))
		if err != nil {
			return "", fmt.Errorf("decoding base64: %w", err)
		}
	}

	switch {
	case slices.Contains(gz, encoding) || slices.Contains(gzB64, encoding):
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("reading gzip header: %w", err)
		}
		// bound the output, user-data is untrusted
		data, err = io.ReadAll(io.LimitReader(r, maxGunzipSize+1))
		if err != nil {
			return "", fmt.Errorf("decompressing gzip: %w", err)
		}
		if len(data) > maxGunzipSize {
			return "", fmt.Errorf("decompressed content exceeds %d bytes", maxGunzipSize)
		}
	case encoding == "" || encoding == "text/plain" || slices.Contains(b64, encoding):
	default:
		return "", fmt.Errorf("unsupported encoding")
	}

	return string(data), nil
}

// translateCommands converts 'runcmd' items to shell commands. An item is either a string, run
// through the shell as is, or a list of arguments which is executed without shell expansion.
func translateCommands(node *yaml.Node) ([]string, error) {
	var commands []string

	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected a list of commands")
	}

	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			commands = append(commands, escapeTemplate(item.Value))
		case yaml.SequenceNode:
			var args []string
			if err := item.Decode(&args); err != nil {
				return nil, fmt.Errorf("decoding command arguments (line %d): %w", item.Line, err)
			}
			for i := range args {
				args[i] = shellQuote(args[i])
			}
			commands = append(commands, escapeTemplate(strings.Join(args, " ")))
		default:
			return nil, fmt.Errorf("command (line %d) is neither a string nor a list of strings", item.Line)
		}
	}

	return commands, nil
}

func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// escapeTemplate protects a literal string from yip, which renders file contents, commands and
// the hostname as Go templates. cloud-config values are literals, and files such as Helm chart
// values or Kubernetes manifests legitimately include '{{ ... }}' sequences.
func escapeTemplate(s string) string {
	return strings.ReplaceAll(s, "{{", `{{"{{"}}`)
}
