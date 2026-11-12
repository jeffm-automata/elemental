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

package cloudinit_test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"strings"
	"text/template"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rancher/yip/pkg/schema"

	"github.com/suse/elemental/v3/internal/cloudinit"
)

// capiUserData has the shape of the user-data a machine gets from the Cluster API provider
// for Harvester: the provider settings (package installation, a list form runcmd item and the
// SSH key) merged with the RKE2 bootstrap provider document (files and string runcmd items).
const capiUserData = `#cloud-config
package_update: true
packages:
  - qemu-guest-agent
  - iptables
bootcmd:
  - echo early
ssh_authorized_keys:
  - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForTests capi
write_files:
  - path: /etc/rancher/rke2/config.yaml
    owner: root:root
    permissions: '0640'
    content: |
      token: secret
      server: https://192.168.1.10:9345
  - path: /var/lib/rancher/rke2/server/manifests/chart.yaml
    content: |
      name: {{ .Release.Name }}
runcmd:
  - - systemctl
    - enable
    - --now
    - qemu-guest-agent.service
  - 'INSTALL_RKE2_ARTIFACT_PATH=/opt/rke2-artifacts sh /opt/install.sh'
  - 'systemctl enable rke2-server.service'
  - 'echo success > /run/cluster-api/bootstrap-success.complete'
`

func render(s string) string {
	// yip renders commands and file contents as Go templates
	tmpl, err := template.New("test").Parse(s)
	Expect(err).NotTo(HaveOccurred())
	var out bytes.Buffer
	Expect(tmpl.Execute(&out, map[string]any{})).To(Succeed())
	return out.String()
}

var _ = Describe("Cloud-config translation", func() {
	It("detects cloud-config documents", func() {
		Expect(cloudinit.IsCloudConfig([]byte("#cloud-config\nhostname: foo"))).To(BeTrue())
		Expect(cloudinit.IsCloudConfig([]byte("\n## template: jinja\n#cloud-config\nhostname: foo"))).To(BeTrue())
		Expect(cloudinit.IsCloudConfig([]byte("#cloud-config  \r\nhostname: foo"))).To(BeTrue())
		Expect(cloudinit.IsCloudConfig([]byte("hostname: foo\n#cloud-config"))).To(BeFalse())
		Expect(cloudinit.IsCloudConfig([]byte("#!/bin/sh\necho"))).To(BeFalse())
		Expect(cloudinit.IsCloudConfig([]byte(`{"ignition": {"version": "3.4.0"}}`))).To(BeFalse())
		Expect(cloudinit.IsCloudConfig(nil)).To(BeFalse())
	})

	It("fails on documents without the cloud-config header", func() {
		_, _, err := cloudinit.TranslateCloudConfig([]byte("hostname: foo"))
		Expect(err).To(MatchError(ContainSubstring("missing the '#cloud-config' header")))
	})

	It("fails on invalid YAML", func() {
		_, _, err := cloudinit.TranslateCloudConfig([]byte("#cloud-config\nhostname: [foo"))
		Expect(err).To(MatchError(ContainSubstring("parsing cloud-config document")))
	})

	It("translates the user-data of a Cluster API machine on Harvester", func() {
		cfg, warnings, err := cloudinit.TranslateCloudConfig([]byte(capiUserData))
		Expect(err).NotTo(HaveOccurred())

		By("reporting every key which is not applied")
		Expect(warnings).To(HaveLen(3))
		Expect(warnings).To(ContainElement(ContainSubstring("unsupported cloud-config key 'bootcmd' (line 7)")))
		Expect(warnings).To(ContainElement(ContainSubstring("unsupported cloud-config key 'package_update'")))
		Expect(warnings).To(ContainElement(ContainSubstring("unsupported cloud-config key 'packages'")))

		By("applying keys and files before the network is configured")
		Expect(cfg.Stages).To(HaveLen(2))
		early := cfg.Stages[cloudinit.StageEarly]
		Expect(early).To(HaveLen(1))
		Expect(early[0].SSHKeys).To(Equal(map[string][]string{
			"root": {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForTests capi"},
		}))
		Expect(early[0].Files).To(HaveLen(2))
		Expect(early[0].Files[0].Path).To(Equal("/etc/rancher/rke2/config.yaml"))
		Expect(early[0].Files[0].Permissions).To(Equal(uint32(0o640)))
		Expect(early[0].Files[0].OwnerString).To(Equal("root:root"))
		Expect(early[0].Files[0].Content).To(Equal("token: secret\nserver: https://192.168.1.10:9345\n"))

		By("keeping template expressions in files literal and defaulting permissions")
		Expect(early[0].Files[1].Permissions).To(Equal(uint32(0o644)))
		Expect(early[0].Files[1].Content).NotTo(Equal("name: {{ .Release.Name }}\n"))
		Expect(render(early[0].Files[1].Content)).To(Equal("name: {{ .Release.Name }}\n"))

		By("running commands once the network is online")
		network := cfg.Stages[cloudinit.StageNetwork]
		Expect(network).To(HaveLen(1))
		Expect(network[0].Commands).To(Equal([]string{
			"'systemctl' 'enable' '--now' 'qemu-guest-agent.service'",
			"INSTALL_RKE2_ARTIFACT_PATH=/opt/rke2-artifacts sh /opt/install.sh",
			"systemctl enable rke2-server.service",
			"echo success > /run/cluster-api/bootstrap-success.complete",
		}))
	})

	It("documents why the yip cloud-config loader is not used", func() {
		// If this starts to fail the yip loader got support for list form runcmd items and
		// the need for TranslateCloudConfig should be reviewed.
		_, err := schema.Load(capiUserData, nil, nil, nil)
		Expect(err).To(HaveOccurred())

		// A document with a jinja header is not seen as cloud-config by yip: nothing is applied
		cfg, err := schema.Load("## template: jinja\n#cloud-config\nhostname: foo\n", nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Stages).To(BeEmpty())
	})

	It("translates hostname and users", func() {
		cfg, warnings, err := cloudinit.TranslateCloudConfig([]byte(`## template: jinja
#cloud-config
hostname: node1
users:
  - default
  - name: admin
    passwd: $6$hash
    lock_passwd: false
    groups: wheel, users
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - ssh-ed25519 AAAAadmin admin
      - github:someone
      - https://example.com/key.pub
  - name: svc
    groups: [svc]
    system: true
ssh_authorized_keys:
  - ssh-rsa AAAAroot root
`))
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(HaveLen(4))
		Expect(warnings).To(ContainElement(ContainSubstring("ignoring 'users' item 'default' (line 5)")))
		Expect(warnings).To(ContainElement(ContainSubstring("unsupported 'sudo' setting of user 'admin'")))
		Expect(warnings).To(ContainElement(ContainSubstring("'github:someone' is not a public key line")))
		Expect(warnings).To(ContainElement(ContainSubstring("'https://example.com/key.pub' is not a public key line")))

		early := cfg.Stages[cloudinit.StageEarly]
		Expect(early).To(HaveLen(1))
		Expect(early[0].Hostname).To(Equal("node1"))
		Expect(early[0].Users).To(HaveLen(2))

		admin := early[0].Users["admin"]
		Expect(admin.PasswordHash).To(Equal("$6$hash"))
		Expect(admin.LockPasswd).To(BeFalse())
		Expect(admin.Groups).To(Equal([]string{"wheel", "users"}))
		Expect(admin.Shell).To(Equal("/bin/bash"))

		svc := early[0].Users["svc"]
		Expect(svc.LockPasswd).To(BeTrue())
		Expect(svc.System).To(BeTrue())
		Expect(svc.Groups).To(Equal([]string{"svc"}))

		Expect(early[0].SSHKeys).To(Equal(map[string][]string{
			"admin": {"ssh-ed25519 AAAAadmin admin"},
			"root":  {"ssh-rsa AAAAroot root"},
		}))
		Expect(cfg.Stages).NotTo(HaveKey(cloudinit.StageNetwork))
	})

	It("warns about jinja expressions", func() {
		_, warnings, err := cloudinit.TranslateCloudConfig([]byte(
			"## template: jinja\n#cloud-config\nhostname: '{{ ds.meta_data.local_hostname }}'\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("jinja rendering is not supported")))
	})

	It("decodes file contents", func() {
		var gz bytes.Buffer
		w := gzip.NewWriter(&gz)
		_, err := w.Write([]byte("compressed {{ content }}"))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Close()).To(Succeed())

		doc := strings.Join([]string{
			"#cloud-config",
			"write_files:",
			"  - path: /b64",
			"    encoding: b64",
			"    permissions: 0600",
			"    content: " + base64.StdEncoding.EncodeToString([]byte("plain")),
			"  - path: /gzb64",
			"    encoding: gz+b64",
			"    content: " + base64.StdEncoding.EncodeToString(gz.Bytes()),
			"  - path: /appended",
			"    append: true",
			"    content: foo",
			"  - path: /deferred",
			"    defer: true",
			"    content: foo",
		}, "\n")

		cfg, warnings, err := cloudinit.TranslateCloudConfig([]byte(doc))
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(HaveLen(2))
		Expect(warnings).To(ContainElement(ContainSubstring("skipping file '/appended'")))
		Expect(warnings).To(ContainElement(ContainSubstring("file '/deferred': 'defer: true' is not supported")))

		files := cfg.Stages[cloudinit.StageEarly][0].Files
		Expect(files).To(HaveLen(3))
		Expect(files[0].Content).To(Equal("plain"))
		Expect(files[0].Permissions).To(Equal(uint32(0o600)))
		Expect(files[0].Encoding).To(BeEmpty())
		Expect(render(files[1].Content)).To(Equal("compressed {{ content }}"))
		Expect(files[2].Path).To(Equal("/deferred"))
	})

	DescribeTable("fails with a detailed error on invalid settings",
		func(doc, expected string) {
			_, _, err := cloudinit.TranslateCloudConfig([]byte("#cloud-config\n" + doc))
			Expect(err).To(MatchError(ContainSubstring(expected)))
		},
		Entry("file without path", "write_files:\n  - content: foo\n",
			"parsing cloud-config key 'write_files' (line 3): file number 1 is missing the 'path' field"),
		Entry("invalid permissions", "write_files:\n  - path: /foo\n    permissions: rw-r--r--\n",
			"file '/foo' has invalid permissions 'rw-r--r--'"),
		Entry("invalid base64", "write_files:\n  - path: /foo\n    encoding: b64\n    content: '!!!'\n",
			"decoding content of file '/foo' (encoding 'b64'): decoding base64"),
		Entry("invalid gzip", "write_files:\n  - path: /foo\n    encoding: gzip\n    content: foo\n",
			"decoding content of file '/foo' (encoding 'gzip'): reading gzip header"),
		Entry("unknown encoding", "write_files:\n  - path: /foo\n    encoding: rot13\n    content: foo\n",
			"decoding content of file '/foo' (encoding 'rot13'): unsupported encoding"),
		Entry("user without name", "users:\n  - gecos: nobody\n",
			"parsing cloud-config key 'users' (line 3): user (line 3) is missing the 'name' field"),
		Entry("users is not a list", "users: admin\n", "expected a list of users"),
		Entry("runcmd is not a list", "runcmd: echo\n", "expected a list of commands"),
		Entry("runcmd item is a map", "runcmd:\n  - foo: bar\n", "command (line 3) is neither a string nor a list of strings"),
		Entry("hostname is a list", "hostname: [a, b]\n", "parsing cloud-config key 'hostname' (line 2)"),
	)

	It("quotes list form commands so they are not expanded by the shell", func() {
		cfg, _, err := cloudinit.TranslateCloudConfig([]byte(
			"#cloud-config\nruncmd:\n  - [echo, \"it's $HOME\", '{{ a }}']\n  - echo $HOME\n"))
		Expect(err).NotTo(HaveOccurred())
		cmds := cfg.Stages[cloudinit.StageNetwork][0].Commands
		Expect(cmds).To(HaveLen(2))
		Expect(render(cmds[0])).To(Equal(`'echo' 'it'\''s $HOME' '{{ a }}'`))
		Expect(cmds[1]).To(Equal("echo $HOME"))
	})
})
