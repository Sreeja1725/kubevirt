#!/bin/bash -xe

K8S_VERSION=k8s-1.31
CONT=${K8S_VERSION}-dnsmasq
IMAGE=$1

PORT=$(sudo podman port k8s-1.31-dnsmasq 5000 |awk -F ":" '{print $2}')
IMAGE_PUSH=localhost:${PORT}/${IMAGE}
sudo podman rmi ${IMAGE_PUSH} || true
sudo podman tag ${IMAGE} ${IMAGE_PUSH}
sudo podman push --tls-verify=false ${IMAGE_PUSH}
