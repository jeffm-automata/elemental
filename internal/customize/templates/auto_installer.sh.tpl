#!/bin/bash

mkdir -p /etc/systemd/system/serial-getty@ttyS0.service.d

cat > /etc/systemd/system/serial-getty@ttyS0.service.d/override.conf << EOF
[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin root --noclear %I $TERM
EOF

mkdir -p /etc/systemd/system/getty@tty1.service.d

cat > /etc/systemd/system/getty@tty1.service.d/override.conf << EOF
[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin root --noclear %I $TERM
EOF

# Ensure extensions included in ISO's /extensions folder are loaded at boot
# ISO filesystem is mounted at /run/initramfs/live folder
rm -rf /run/extensions
ln -s /run/initramfs/live/extensions /run/extensions

{{- if .PreloadImages }}
# Preloaded container images: the virtual machine that installs a preinstalled image at build
# time has two more disks, one with the files of the Kubernetes runtime image and one with an OCI
# archive of the container images. After the installation the container runtime of the first
# imports the second into the image store of the installed system, at the place and in the state
# the node's own container runtime would leave them after pulling. On any other machine, for
# instance when the system is reset from its recovery partition, there are no such disks and the
# script does nothing.
cat > /etc/elemental-preload-images.sh << 'PRELOAD'
#!/bin/bash
set -uo pipefail

runtime="/dev/disk/by-id/virtio-{{ .RuntimeDisk }}"
images="/dev/disk/by-id/virtio-{{ .ImagesDisk }}"
[[ -b "${runtime}" && -b "${images}" ]] || exit 0

work=/run/elemental-preload
rke2=/var/lib/rancher/rke2
store="${rke2}/agent/containerd"
sock="${work}/containerd.sock"
pid=""

# The serial port of the virtual machine is the only way out: the image build reads the result
# there. It is written to directly, whatever consoles the kernel was told to use.
say() {
  local port
  for port in /dev/ttyS0 /dev/ttyAMA0; do
    [[ -c "${port}" ]] && echo "{{ .LogPrefix }} $*" 2> /dev/null > "${port}"
  done
  return 0
}

finish() {
  if [[ -n "${pid}" ]]; then
    kill "${pid}" 2>/dev/null
    wait "${pid}" 2>/dev/null
  fi
  sync
  umount /var/lib/rancher 2>/dev/null
  umount "${work}/var" 2>/dev/null
}

fail() {
  say "$*"
  tail -n 15 "${work}/log" 2>/dev/null | while read -r line; do say "${line}"; done
  finish
  exit 1
}

mkdir -p "${work}/var" /var/lib/rancher
mount -o "subvol={{ .VarSubvolume }}" "/dev/disk/by-label/{{ .SystemLabel }}" "${work}/var" > "${work}/log" 2>&1 \
  || fail "cannot mount /var of the installed system"
mkdir -p "${work}/var/lib/rancher"
# The store is filled at the path it has on a node, so that paths and SELinux labels are the node's.
mount --bind "${work}/var/lib/rancher" /var/lib/rancher >> "${work}/log" 2>&1 \
  || fail "cannot mount the image store at its path"

mkdir -p "${rke2}/data" "${store}/io.containerd.snapshotter.v1.overlayfs/snapshots"
chmod 0750 "${rke2}/agent"
tar -C "${rke2}/data" -xpf "${runtime}" >> "${work}/log" 2>&1 \
  || fail "cannot extract the Kubernetes runtime"
bin=$(echo "${rke2}"/data/*/bin)
[[ -x "${bin}/containerd" && -x "${bin}/ctr" ]] || fail "the Kubernetes runtime has no containerd"
# Layers are unpacked below directories that carry their label already, as on a node.
if command -v restorecon > /dev/null; then restorecon -R /var/lib/rancher >> "${work}/log" 2>&1; fi

cat > "${work}/config.toml" << TOML
root = "${store}"
state = "${work}/state"
[grpc]
  address = "${sock}"
TOML

PATH="${bin}:${PATH}" "${bin}/containerd" --config "${work}/config.toml" > "${work}/containerd.log" 2>&1 &
pid=$!
ctr() { "${bin}/ctr" --address "${sock}" --namespace k8s.io "$@"; }
for _ in $(seq 60); do ctr version > /dev/null 2>&1 && break; sleep 1; done
ctr version > /dev/null 2>&1 || { tail -n 15 "${work}/containerd.log" >> "${work}/log"; fail "containerd did not start"; }

ctr images import "${images}" >> "${work}/log" 2>&1 || fail "cannot import the images"

count=$(ctr images ls -q | grep -vc '^sha256:')
ctr images check > "${work}/check" 2>> "${work}/log"
packed=$(awk 'NR > 1 && $NF != "true"' "${work}/check" | wc -l)
if [[ "${count}" -eq 0 || "${packed}" -ne 0 ]]; then
  cat "${work}/check" >> "${work}/log"
  fail "${count} images imported, ${packed} of them not unpacked"
fi
snapshots=$(ctr snapshots ls | awk 'NR > 1' | wc -l)

kill "${pid}"; wait "${pid}" 2>/dev/null; pid=""
if command -v restorecon > /dev/null; then restorecon -R /var/lib/rancher >> "${work}/log" 2>&1; fi
size=$(du -sm "${rke2}" | cut -f1)
finish
say "{{ .DoneMark }} ${count} images, ${snapshots} layers, ${size} MiB in the image store of the installed system"
PRELOAD
{{- end }}

cat > /etc/systemd/system/elemental-autoinstall.service << EOF
[Unit]
Description=Elemental Autoinstall
After=multi-user.target
ConditionPathExists=/run/initramfs/live/Install/install.yaml
ConditionFileIsExecutable=/usr/bin/elemental3ctl
{{- if eq .MediaType "raw" }}
ConditionKernelCommandLine=elm.recovery
ConditionKernelCommandLine=elm.reset
{{- end }}
OnSuccess=reboot.target
StartLimitIntervalSec=600
StartLimitBurst=3

[Service]
Type=oneshot
{{- if eq .MediaType "iso" }}
ExecStart=/usr/bin/elemental3ctl --debug install
{{- else }}
ExecStart=/usr/bin/elemental3ctl --debug reset
{{- if .PreloadImages }}
ExecStartPost=-/bin/bash /etc/elemental-preload-images.sh
{{- end }}
{{- end }}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl enable elemental-autoinstall.service
