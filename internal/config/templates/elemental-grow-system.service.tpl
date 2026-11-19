[Unit]
Description=Elemental: grow the system partition and file system to the size of the disk
After=local-fs.target
Before=k8s-config-installer.service rke2-server.service rke2-agent.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh "{{ .Script }}"

[Install]
WantedBy=multi-user.target
