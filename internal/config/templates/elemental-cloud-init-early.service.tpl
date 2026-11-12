[Unit]
Description=Elemental cloud-init: datasource, users, files and hostname
DefaultDependencies=no
After=local-fs.target
Wants=local-fs.target network-pre.target
# The hostname has to be set before the network is configured, and the node role and
# the Kubernetes configuration have to be written before Kubernetes is installed.
Before=sysinit.target network-pre.target {{ .K8sUnit }} shutdown.target
Conflicts=shutdown.target
ConditionPathExists={{ .Binary }}

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart={{ .Binary }} cloud-init --fetch --stage initramfs --stage fs

[Install]
WantedBy=sysinit.target
