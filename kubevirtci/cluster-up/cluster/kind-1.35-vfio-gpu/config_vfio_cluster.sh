#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Post-creation per-node configuration for the kind-vfio-gpu cluster.
#
# Mirrors kubevirtci's cluster-up/cluster/kind-1.35-vgpu/
# config_vgpu_cluster.sh + vgpu-node/node.sh:
#
#   - node::remount_sysfs:    kind nodes mount /sys read-only by default,
#                             which blocks any sysfs writes that consumer
#                             pods or kubelet may need (PCI driver bind,
#                             vfio group membership tweaks, etc.). Remount
#                             rw inside every node.
#   - node::chmod_vfio_vfio:  /dev/vfio/vfio is the VFIO container device
#                             that every VFIO user must open. By default
#                             it is mode 0600 (root:root), so unprivileged
#                             pods cannot use it. chmod 666 keeps it
#                             accessible from pods that don't need full
#                             host privileges.
#   - node::discover_devices: prints the synthetic vfio-pci devices each
#                             node can see, so failures in step 1/2 are
#                             obvious from the log.
#
# Inputs (env):
#   KIND_CLUSTER_NAME    name of the kind cluster (required)
#   CONTAINER_TOOL       docker | podman (default: docker)

set -e
set -o pipefail

: "${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME is required}"
: "${CONTAINER_TOOL:=docker}"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'
log_info() { printf "${GREEN}[INFO]${NC} %s\n" "$*"; }
log_warn() { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }
log_err()  { printf "${RED}[ERROR]${NC} %s\n" "$*" >&2; }

# Resolve the kind node container names for this cluster. kind names them
# <cluster>-control-plane, <cluster>-worker, etc.
list_nodes() {
    "${CONTAINER_TOOL}" ps --format '{{.Names}}' \
        --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}"
}

node::remount_sysfs() {
    local node="$1"
    log_info "[${node}] mount -o remount,rw /sys"
    "${CONTAINER_TOOL}" exec "${node}" mount -o remount,rw /sys
}

node::chmod_vfio_vfio() {
    local node="$1"
    if "${CONTAINER_TOOL}" exec "${node}" test -c /dev/vfio/vfio; then
        log_info "[${node}] chmod 666 /dev/vfio/vfio"
        "${CONTAINER_TOOL}" exec "${node}" chmod 666 /dev/vfio/vfio
    else
        log_warn "[${node}] /dev/vfio/vfio not present - check that fake-iommu is loaded on the host" \
                 "and that the kind config bind-mounts /dev/vfio into the node."
    fi
}

node::discover_devices() {
    local node="$1"
    log_info "[${node}] vfio-pci devices visible inside the node:"
    "${CONTAINER_TOOL}" exec "${node}" bash -c '
        found=0
        for d in /sys/bus/pci/drivers/vfio-pci/*; do
            bdf=$(basename "$d")
            [[ "$bdf" == "bind"   ]] && continue
            [[ "$bdf" == "unbind" ]] && continue
            [[ "$bdf" == "new_id" ]] && continue
            [[ "$bdf" == "remove_id" ]] && continue
            [[ "$bdf" == "module" ]] && continue
            [[ "$bdf" == "uevent" ]] && continue
            v=$(cat /sys/bus/pci/devices/$bdf/vendor 2>/dev/null || echo ?)
            dev=$(cat /sys/bus/pci/devices/$bdf/device 2>/dev/null || echo ?)
            grp=$(basename "$(readlink /sys/bus/pci/devices/$bdf/iommu_group 2>/dev/null)" 2>/dev/null)
            echo "  $bdf  vendor=$v device=$dev iommu_group=${grp:-none}"
            found=$((found + 1))
        done
        if [[ $found -eq 0 ]]; then
            echo "  (none)"
        fi
        echo "  /dev/vfio entries:"
        ls -1 /dev/vfio/ 2>/dev/null | sed "s/^/    /" || echo "    (none)"
    '
}

main() {
    local nodes
    mapfile -t nodes < <(list_nodes)

    if [[ ${#nodes[@]} -eq 0 ]]; then
        log_err "No nodes found for kind cluster '${KIND_CLUSTER_NAME}'."
        log_err "Is the cluster running? Try: ${CONTAINER_TOOL} ps --filter label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}"
        exit 1
    fi

    log_info "Configuring ${#nodes[@]} node(s): ${nodes[*]}"

    local n
    for n in "${nodes[@]}"; do
        node::remount_sysfs   "${n}"
        node::chmod_vfio_vfio "${n}"
    done

    for n in "${nodes[@]}"; do
        node::discover_devices "${n}"
    done
}

main "$@"
