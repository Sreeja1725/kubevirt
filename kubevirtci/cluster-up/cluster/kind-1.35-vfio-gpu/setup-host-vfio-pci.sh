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
#
# Builds fake-pci/fake-iommu modules and loads them on the Linux host.
# Run before: make cluster-up KUBEVIRT_PROVIDER=kind-1.35-vfio-gpu

set -e

SCRIPT_PATH=$(dirname "$(realpath "$0")")
VFIO_DIR="${SCRIPT_PATH}"

: "${FAKE_PCI_DEVICES:=8}"

log() { echo "[setup-host-vfio-pci] $*"; }

if [[ "$(uname -s)" != "Linux" ]]; then
    echo "ERROR: synthetic vfio-pci host setup requires Linux."
    exit 1
fi

if [ ! -x "${VFIO_DIR}/setup-fake-pci-host.sh" ]; then
    echo "ERROR: ${VFIO_DIR}/setup-fake-pci-host.sh not found"
    exit 1
fi

log "Building fake-pci and fake-iommu modules (FAKE_PCI_DEVICES=${FAKE_PCI_DEVICES})"
make -C "${VFIO_DIR}/fake-pci"
make -C "${VFIO_DIR}/fake-iommu"

ACTION="${1:-setup}"
case "${ACTION}" in
    setup)
        export FAKE_PCI_DEVICES
        sudo "${VFIO_DIR}/setup-fake-pci-host.sh" setup
        sudo "${VFIO_DIR}/setup-fake-pci-host.sh" bind-vfio
        ;;
    cleanup|status|bind-vfio)
        sudo "${VFIO_DIR}/setup-fake-pci-host.sh" "${ACTION}"
        ;;
    *)
        echo "Usage: $0 [setup|cleanup|status|bind-vfio]"
        exit 1
        ;;
esac

log "Done (${ACTION})"
