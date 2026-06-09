# Kind 1.35 cluster with DRA vfio-gpu for KubeVirt e2e

Provides an ephemeral Kind cluster (Kubernetes 1.35) wired for the `vfio-gpu`
profile of [dra-example-driver](https://github.com/kubernetes-sigs/dra-example-driver),
using **synthetic** PCI devices from the `fake-iommu` and `fake-pci` kernel
modules on the Linux host. KubeVirt is built locally and pushed to the in-cluster
registry at `localhost:5000`.

> **Linux host only.** `fake-iommu.ko` and `fake-pci.ko` must be loaded into
> the live host kernel. macOS Docker Desktop and remote-Docker setups cannot
> load custom kernel modules.

## Contents

| File / dir               | Purpose                                                             |
| -------------------------| --------------------------------------------------------------------|
| `fake-iommu/`            | Kernel module that exposes a fake IOMMU group                       |
| `fake-pci/`              | Kernel module that publishes synthetic PCI devices on bus `faca`    |
| `setup-fake-pci-host.sh` | Load/unload modules and bind devices to `vfio-pci`                  |
| `setup-host-vfio-pci.sh` | Build modules + run host setup (wrapper used before `cluster-up`)   |
| `kind.yaml`              | Kind config (DRA feature gates, CDI, CPUManager, mounts)            |
| `config_vfio_cluster.sh` | Post-create per-node tweaks (`/sys` remount, `/dev/vfio` perms)     |
| `provider.sh`            | kubevirtci provider entry point (`make cluster-up` / `cluster-down`)|

## Prerequisites

Linux host with kernel headers, `docker`, `kind`, `kubectl`, `helm`, `git`, `make` and `sudo`.

Debian/Ubuntu: `sudo apt-get install linux-headers-$(uname -r)`  
Fedora/RHEL: `sudo dnf install kernel-devel-$(uname -r)`

## Host setup (before cluster-up)

Build the kernel modules and bind synthetic devices to `vfio-pci`:

```bash
export FAKE_PCI_DEVICES=8
export FAKE_IOMMU=true
bash kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu/setup-host-vfio-pci.sh
```
or

When calling `setup-fake-pci-host.sh` directly, pass env vars **before** the
command (not as positional arguments). `sudo` does not preserve a prior
`export` unless you use `-E` or inline assignment:

```bash
sudo FAKE_PCI_DEVICES=8 bash kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu/setup-fake-pci-host.sh setup
sudo bash kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu/setup-fake-pci-host.sh bind-vfio
```

Verify:

```bash
ls /sys/bus/pci/drivers/vfio-pci/
```

To change the device count after a previous setup, run `cleanup` first:

```bash
sudo bash kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu/setup-fake-pci-host.sh cleanup
export FAKE_PCI_DEVICES=8
export FAKE_IOMMU=true
bash kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu/setup-host-vfio-pci.sh
```

To unload the modules later without tearing down Kind:

```bash
sudo bash kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu/setup-fake-pci-host.sh cleanup
```

## Cluster and KubeVirt

From the kubevirt repo root:

```bash
export KUBEVIRT_PROVIDER=kind-1.35-vfio-gpu
export FEATURE_GATES=GPUsWithDRA,HostDevicesWithDRA,HostDevices
make cluster-up
make cluster-sync
```

`cluster-up` creates Kind, bind-mounts `/dev/vfio` and PCI sysfs into nodes,
and runs `config_vfio_cluster.sh`. 
— use `cluster-sync` for KubeVirt (with the feature gates above).

## Run DRA e2e

Once the driver is installed and KubeVirt is synced with DRA feature gates:

```bash
export KUBEVIRT_PROVIDER=kind-1.35-vfio-gpu
export FEATURE_GATES=GPUsWithDRA,HostDevicesWithDRA,HostDevices
export KUBEVIRT_E2E_FOCUS='\[sig-compute\]DRA'
make ginkgo
```

## Tear down

```bash
export KUBEVIRT_PROVIDER=kind-1.35-vfio-gpu
make cluster-down
```

This deletes the Kind cluster and, on Linux, unloads `fake-pci` and `fake-iommu`
from the host. Set `CLEANUP_FAKE_PCI=false` to leave the modules loaded.

## Configuration

| Variable           | Default | Purpose |
| ------------------ | ------- | ------- |
| `FAKE_PCI_DEVICES` | `8` in `setup-host-vfio-pci.sh` | Number of synthetic PCI devices on the host |
| `USE_FAKE_PCI`     | `true` | Warn at `cluster-up` if no `vfio-pci` devices are bound |
| `CLEANUP_FAKE_PCI` | `true` | Unload host kernel modules on `cluster-down` |
| `FEATURE_GATES`    | (none) | Set to `GPUsWithDRA,HostDevicesWithDRA,HostDevices` before `cluster-sync` |
| `KIND_NODE_IMAGE` / `KIND_VERSION` | from `image` / `version` files | Override Kind node image or binary version |

## Setting a custom Kind version

```bash
export KIND_NODE_IMAGE="kindest/node:v1.35.0@sha256:..."
export KIND_VERSION="0.31.0"
export KUBEVIRT_PROVIDER=kind-1.35-vfio-gpu
make cluster-up
```

See https://github.com/kubernetes-sigs/kind/releases for node images per Kind version.
