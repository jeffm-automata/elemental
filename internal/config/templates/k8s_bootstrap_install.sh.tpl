#!/bin/sh

# Install hook for Kubernetes bootstrap providers.
#
# The RKE2 bootstrap provider of Cluster API in air-gapped mode, like anything else written
# for the upstream RKE2 installer, runs "sh {{ .HookPath }}" with INSTALL_RKE2_TYPE set to
# "agent" on workers and unset on servers, and then enables and starts the RKE2 service
# itself. RKE2 is part of this image, so nothing is downloaded: the hook records the node
# role and runs the same installation as k8s-config-installer.service, without starting RKE2.

set -eu

RUNTIME_ENV="{{ .RuntimeEnvPath }}"
RKE2_CONFIG=/etc/rancher/rke2/config.yaml

if [ -x "{{ .RKE2Binary }}" ]; then
  echo "RKE2 is already installed from the image"
  exit 0
fi

NODETYPE="${INSTALL_RKE2_TYPE:-server}"
case "${NODETYPE}" in
  server|agent) ;;
  *)
    echo "Error: unsupported INSTALL_RKE2_TYPE '${NODETYPE}'" >&2
    exit 1
    ;;
esac

# A server that is not told which server to join is the one that initialises the cluster.
IS_INIT_NODE=false
if [ "${NODETYPE}" = "server" ] \
  && ! grep -qsE '^server:' "${RKE2_CONFIG}" "${RKE2_CONFIG}.d"/*.yaml; then
  IS_INIT_NODE=true
fi

mkdir -p "$(dirname "${RUNTIME_ENV}")"
printf 'NODETYPE=%s\nIS_INIT_NODE=%s\n' "${NODETYPE}" "${IS_INIT_NODE}" > "${RUNTIME_ENV}"

export NODETYPE IS_INIT_NODE
START_RKE2=false /bin/bash "{{ .ConfigDeployScript }}"

# The resource installer was skipped at boot, when the role was not known yet.
if [ "${IS_INIT_NODE}" = "true" ] && [ -e "/etc/systemd/system/{{ .ResourcesUnit }}" ]; then
  systemctl start --no-block "{{ .ResourcesUnit }}"
fi
