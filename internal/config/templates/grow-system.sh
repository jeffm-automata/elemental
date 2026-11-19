#!/bin/sh

# A preinstalled image is as big as it was built. The disk of the machine may be bigger:
# grow the system partition, which is the last one, and its file system to the end of the disk.
# Runs on every boot and changes nothing when there is no free space after the partition.

set -eu

part="$(findmnt --noheadings --nofsroot --output SOURCE /)"
disk="/dev/$(lsblk --noheadings --output PKNAME "${part}" | head -n 1)"

defs="$(mktemp -d /run/elemental-grow.XXXXXX)"
trap 'rm -rf "${defs}"' EXIT

# Partitions without a definition are left as they are.
printf '[Partition]\nType=root\n' > "${defs}/50-system.conf"

systemd-repart --dry-run=no --definitions="${defs}" "${disk}"

# The root of the system is a read-only snapshot. A file system is grown through any of its
# mount points, as long as that one is writable: take the first read-write volume.
rw="$(findmnt --raw --noheadings --nofsroot --source "${part}" --options rw --output TARGET | head -n 1)"
/usr/lib/systemd/systemd-growfs "${rw:-/}"
