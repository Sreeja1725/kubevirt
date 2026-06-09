#!/bin/bash -e
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
# Copyright 2024 Red Hat, Inc.
#

DRA_EXAMPLE_DRIVER_REPO="https://github.com/Sreeja1725/dra-example-driver.git"
DRA_EXAMPLE_DRIVER_BRANCH="kubevirt-dra-profile"
DRA_EXAMPLE_DRIVER_DIR=${DRA_EXAMPLE_DRIVER_DIR:-"${KUBEVIRTCI_CONFIG_PATH}/${KUBEVIRT_PROVIDER}/_dra-example-driver"}

function cluster::_get_dra_repo() {
    git --git-dir "${DRA_EXAMPLE_DRIVER_DIR}/.git" config --get remote.origin.url 2>/dev/null || true
}

function cluster::_dra_driver_image_tag() {
    grep '^appVersion:' "${DRA_EXAMPLE_DRIVER_DIR}/deployments/helm/dra-example-driver/Chart.yaml" \
        | sed -E 's/^appVersion:[[:space:]]*"?([^"]*)"?/\1/'
}

function cluster::clone_dra_example_driver() {
    if [ -d "${DRA_EXAMPLE_DRIVER_DIR}" ]; then
        if [ "$(cluster::_get_dra_repo)" != "${DRA_EXAMPLE_DRIVER_REPO}" ]; then
            rm -rf "${DRA_EXAMPLE_DRIVER_DIR}"
        fi
    fi

    if [ ! -d "${DRA_EXAMPLE_DRIVER_DIR}" ]; then
        git clone --depth 1 --branch "${DRA_EXAMPLE_DRIVER_BRANCH}" \
            "${DRA_EXAMPLE_DRIVER_REPO}" "${DRA_EXAMPLE_DRIVER_DIR}"
    fi
}

function cluster::install_dra_example_driver() {
    : "${DRA_DRIVER_PROFILE:=vfio-gpu}"
    : "${DRA_DRIVER_NAME:=vfio-gpu.example.com}"
    : "${KIND_CLUSTER_NAME:=${CLUSTER_NAME}}"

    cluster::clone_dra_example_driver

    local driver_image_tag
    driver_image_tag=$(cluster::_dra_driver_image_tag)
    if [ -z "${driver_image_tag}" ]; then
        echo "ERROR: could not determine DRA driver image tag from Chart.yaml" >&2
        exit 1
    fi

    # build-driver.sh derives DRIVER_IMAGE_TAG via git rev-parse, which resolves
    # to the wrong repo when invoked from kubevirt's cluster-up cwd.
    export KIND_CLUSTER_NAME CONTAINER_TOOL DRIVER_IMAGE_TAG="${driver_image_tag}"
    (
        cd "${DRA_EXAMPLE_DRIVER_DIR}"
        bash demo/build-driver.sh
    )

    helm upgrade -i dra-example-driver "${DRA_EXAMPLE_DRIVER_DIR}/deployments/helm/dra-example-driver" \
        --kubeconfig "${KUBECONFIG}" \
        --namespace dra-example-driver --create-namespace \
        --set deviceProfile="${DRA_DRIVER_PROFILE}" \
        --set driverName="${DRA_DRIVER_NAME}" \
        --set kubeletPlugin.enableDeviceMetadata=true
}
