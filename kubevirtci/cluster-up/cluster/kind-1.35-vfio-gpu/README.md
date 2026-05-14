# Kind 1.35 cluster with DRA vfio-gpu for KubeVirt e2e

This directory brings up a kind cluster wired for the `vfio-gpu` profile of
the example DRA driver, using **synthetic** PCI devices created by the
`fake-iommu` and `fake-pci` kernel modules. KubeVirt is installed on top
with the `GPUsWithDRA` / `HostDevicesWithDRA` feature gates enabled, so
DRA-allocated devices can be claimed by a VMI.

> **Linux host only.** `fake-iommu.ko` and `fake-pci.ko` are kernel
> modules that must be `insmod`'d into the live host kernel. macOS
> Docker Desktop and remote-Docker setups don't expose the host kernel,
> so they can't run this demo.

## Contents

| File / dir                  | Purpose                                                          |
| --------------------------- | ---------------------------------------------------------------- |
| `fake-iommu/`               | Tiny kernel module that exposes a fake IOMMU group                |
| `fake-pci/`                 | Kernel module that publishes 4 synthetic PCI devices on bus `faca` |
| `setup-fake-pci-host.sh`    | Loads the modules and binds the synthetic devices to `vfio-pci`   |
| `kind-cluster-config.yaml`  | kind config (api-server feature gates, mounts, etc.)              |
| `config-vfio-cluster.sh`    | Post-create per-node tweaks (`/sys` remount, `/dev/vfio` perms)   |
| `create-cluster.sh`         | One-shot: pre-flight → build modules → host setup → kind → KubeVirt |
| `delete-cluster.sh`         | Tear down kind + unbind modules                                   |

## Prerequisites

Linux host, `docker`, `kind`, `kubectl`, `helm`, `git`, `make`, kernel headers.

## Host: 8 synthetic PCI devices

```bash
export FAKE_PCI_DEVICES=8
bash kubevirtci/cluster-up/cluster/kind-1.35-vfio-gpu/setup-host-vfio-pci.sh
```

Verify:

```bash
ls /sys/bus/pci/drivers/vfio-pci/
```

## Cluster and KubeVirt

```bash
export KUBEVIRT_PROVIDER=kind-1.35-vfio-gpu
make cluster-up
make cluster-sync
```

`cluster-up` creates Kind, configures nodes (`config_vfio_cluster.sh`), and installs the DRA driver (`deploy-dra-example-driver.sh`).

## Run DRA e2e

```bash
export KUBEVIRT_PROVIDER=kind-1.35-vfio-gpu
export KUBEVIRT_E2E_FOCUS='\[sig-compute\]DRA'
make ginkgo
```

## Configuration

| Variable | Default | Purpose |
| -------- | ------- | ------- |
| `FAKE_PCI_DEVICES` | `8` in setup script | Synthetic PCI count on host |
| `USE_FAKE_PCI` | `true` | Warn if no vfio-pci at cluster-up |
| `KUBEVIRT_DEPLOY_DRA_DRIVER` | `true` | Skip driver Helm install if `false` |
| `DRA_USE_PUBLISHED_DRIVER` | `false` | Use `registry.k8s.io` chart/image when upstream ships vfio-gpu |
| `DRA_EXAMPLE_DRIVER_SRC` | clone under `.kubevirtci` | Local dra-example-driver checkout |
| `DRA_EXAMPLE_DRIVER_REPO` / `DRA_EXAMPLE_DRIVER_REF` | Sreeja1725 / `dra-kubevirt` | Driver source when building locally |

Host vfio-pci setup (`setup-host-vfio-pci.sh`) is still required for synthetic devices.
