[Unit]
Description=Elemental cloud-init: network stage and user commands
# User commands (cloud-config runcmd) are run last, as cloud-init does: once the network is
# online and after the Kubernetes distribution shipped in the image is installed.
After=network-online.target {{ .BootUnit }} {{ .K8sUnit }}
Wants=network-online.target
ConditionPathExists={{ .Binary }}

[Service]
Type=oneshot
RemainAfterExit=yes
TimeoutStartSec=infinity
ExecStart={{ .Binary }} cloud-init --stage network --finalize

[Install]
WantedBy=multi-user.target
