#!/usr/bin/env bash
#
# This file is part of the KubeVirt project
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
#
# Copyright the KubeVirt Authors.

set -e

DEFAULT_CLUSTER_NAME="kind-1.35-vfio-gpu"
DEFAULT_HOST_PORT=5000
ALTERNATE_HOST_PORT=5001
export CLUSTER_NAME=${CLUSTER_NAME:-$DEFAULT_CLUSTER_NAME}

if [ "$CLUSTER_NAME" = "$DEFAULT_CLUSTER_NAME" ]; then
    export HOST_PORT=$DEFAULT_HOST_PORT
else
    export HOST_PORT=$ALTERNATE_HOST_PORT
fi

export USE_FAKE_PCI=${USE_FAKE_PCI:-true}

VFIO_PROVIDER_DIR="${KUBEVIRTCI_PATH}/cluster/${KUBEVIRT_PROVIDER}"

function set_kind_params() {
    version=$(cat "${KUBEVIRTCI_PATH}/cluster/$KUBEVIRT_PROVIDER/version")
    export KIND_VERSION="${KIND_VERSION:-$version}"

    image=$(cat "${KUBEVIRTCI_PATH}/cluster/$KUBEVIRT_PROVIDER/image")
    export KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-$image}"
}

function configure_registry_proxy() {
    [ "$CI" != "true" ] && return

    echo "Configuring cluster nodes to work with CI mirror-proxy..."

    local -r ci_proxy_hostname="docker-mirror-proxy.kubevirt-prow.svc"
    local -r kind_binary_path="${KUBEVIRTCI_CONFIG_PATH}/$KUBEVIRT_PROVIDER/.kind"
    local -r configure_registry_proxy_script="${KUBEVIRTCI_PATH}/cluster/kind/configure-registry-proxy.sh"

    KIND_BIN="$kind_binary_path" PROXY_HOSTNAME="$ci_proxy_hostname" $configure_registry_proxy_script
}

function _maybe_mount() {
    local host_path=$1
    local container_path=${2:-$1}
    if [[ -e "$host_path" ]]; then
        cat <<EOF >> ${KUBEVIRTCI_CONFIG_PATH}/$KUBEVIRT_PROVIDER/kind.yaml
  - containerPath: ${container_path}
    hostPath: ${host_path}
EOF
    else
        echo "  (skipping mount of ${host_path}: not present on host)"
    fi
}

function _add_pci_sysfs_mounts() {
    _maybe_mount /sys/bus/pci
    _maybe_mount /sys/kernel/iommu_groups
}

function warn_if_no_vfio_pci() {
    if [ "$USE_FAKE_PCI" != "true" ]; then
        return 0
    fi

    if [[ "$(uname -s)" != "Linux" ]]; then
        echo ""
        echo "WARNING: vfio-pci devices are discovered from the host sysfs tree."
        echo "On non-Linux hosts the driver will advertise zero devices and DRA e2e will skip."
        echo ""
        return 0
    fi

    if ! ls /sys/bus/pci/drivers/vfio-pci/*:* >/dev/null 2>&1; then
        echo ""
        echo "WARNING: no devices bound to vfio-pci on the host."
        echo "Run (Linux, requires sudo):"
        echo "  bash ${VFIO_PROVIDER_DIR}/setup-host-vfio-pci.sh"
        echo ""
    else
        echo "vfio-pci devices are available on the host"
    fi
}

function up() {
    warn_if_no_vfio_pci

    cp ${KUBEVIRTCI_PATH}/cluster/${KUBEVIRT_PROVIDER}/kind.yaml \
        ${KUBEVIRTCI_CONFIG_PATH}/$KUBEVIRT_PROVIDER/kind.yaml
    _add_extra_mounts
    _add_pci_sysfs_mounts
    _add_extra_portmapping
    export CONFIG_WORKER_CPU_MANAGER=true
    kind_up

    configure_registry_proxy

    export KIND_CLUSTER_NAME="${CLUSTER_NAME}"
    export CONTAINER_TOOL="${CRI_BIN}"
    bash "${VFIO_PROVIDER_DIR}/config_vfio_cluster.sh"

    echo "Installing DRA example driver"
    cluster::install_dra_example_driver

    echo "$KUBEVIRT_PROVIDER cluster '$CLUSTER_NAME' is ready"
}

set_kind_params

source ${KUBEVIRTCI_PATH}/cluster/kind/common.sh
export KUBECONFIG="${KUBEVIRTCI_CONFIG_PATH}/${KUBEVIRT_PROVIDER}/.kubeconfig"
source "${VFIO_PROVIDER_DIR}/install_dra_example_driver.sh"

function cleanup_host_fake_pci() {
    if [ "$USE_FAKE_PCI" != "true" ]; then
        return 0
    fi
    if [ "${CLEANUP_FAKE_PCI:-true}" != "true" ]; then
        echo "CLEANUP_FAKE_PCI=false - leaving fake-iommu / fake-pci loaded on the host"
        return 0
    fi
    if [[ "$(uname -s)" != "Linux" ]]; then
        echo "Not on Linux ($(uname -s)); skipping kernel-module unload"
        return 0
    fi

    local setup_script="${VFIO_PROVIDER_DIR}/setup-fake-pci-host.sh"
    if [ ! -f "${setup_script}" ]; then
        echo "WARNING: ${setup_script} not found; skipping kernel-module unload"
        return 0
    fi

    echo "Unloading fake-pci + fake-iommu kernel modules on the host"
    sudo bash "${setup_script}" cleanup || true
}

function down() {
    _fetch_kind
    if [ -z "$($KIND get clusters | grep ${CLUSTER_NAME})" ]; then
        cleanup_host_fake_pci
        return
    fi

    worker_nodes=$(_get_nodes | grep -i $WORKER_NODES_PATTERN | awk '{print $1}')
    for worker_node in $worker_nodes; do
        if ip netns exec $worker_node ip -details address | grep "vf 0" -B 2 > /dev/null; then
            iface=$(ip netns exec $worker_node ip -details address | grep "vf 0" -B 2 | grep -E 'UP|DOWN' | awk -F": " '{print $2}')
            ip netns exec $worker_node ip link set $iface netns 1 && echo "gracefully detached $iface from $worker_node"
        fi
    done

    # On CI, avoid failing an entire test run just because of a deletion error
    $KIND delete cluster --name=${CLUSTER_NAME} || [ "$CI" = "true" ]
    ${CRI_BIN} rm -f $REGISTRY_NAME >> /dev/null
    rm -f ${KUBEVIRTCI_CONFIG_PATH}/$KUBEVIRT_PROVIDER/kind.yaml

    cleanup_host_fake_pci
}
