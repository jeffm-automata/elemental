[Unit]
Description=Elemental cloud-init: boot stage
After={{ .EarlyUnit }}
ConditionPathExists={{ .Binary }}

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart={{ .Binary }} cloud-init --stage boot

[Install]
WantedBy=multi-user.target
