# Fake IOMMU Companion Module

This kernel module is a no-op IOMMU driver that claims PCI devices on a
specific (synthetic) PCI domain. It is the **prerequisite** for getting
`vfio-pci` to bind to the fake devices created by
[`../fake-nvidia-pci/`](../fake-nvidia-pci/), which in turn lets a KubeVirt
VMI configured for DRA PCI passthrough reach `Running`.

## Why it exists

`vfio-pci`'s `.probe` callback requires `iommu_group_get(&pdev->dev)` to
return a non-NULL group. On a kind host with no real IOMMU backing the
synthetic `0xfaca:` PCI bus, that call returns NULL and vfio-pci refuses to
bind. KubeVirt's virt-handler then fails its pre-start hook, and the VMI
never moves out of `Scheduling`.

This module satisfies vfio-pci's prerequisite by:

1. Registering a software-only `struct iommu_device` with the kernel IOMMU
   framework.
2. Listening on `pci_bus_type` for new devices via a high-priority bus
   notifier (priority 100) that runs *before* the kernel's own IOMMU bus
   notifier (priority 0).
3. Filtering for devices whose PCI domain matches `target_domain`
   (default `0xfaca`).
4. Setting up an `iommu_fwspec` on each matching device that points at this
   module's fwnode. The kernel's own iommu bus notifier then probes the
   device automatically on `BUS_NOTIFY_ADD_DEVICE`, finds our ops via
   `iommu_ops_from_fwnode()`, and wires the device into a per-device IOMMU
   group through our `probe_device` / `device_group` ops callbacks.

We deliberately never call `iommu_probe_device()` directly: it lives in
`drivers/iommu/iommu-priv.h` and is not exported to out-of-tree modules.
Instead we lean on two existing kernel mechanisms to do the probe for us:

- `iommu_device_register()` internally calls `bus_iommu_probe()` for every
  bus the new IOMMU registered on, which walks every device on that bus
  and probes any with a matching fwspec. So before calling it we set
  fwspec on every fake PCI device that already exists.
- For devices that hotplug later, the kernel's per-bus iommu notifier
  fires on `BUS_NOTIFY_ADD_DEVICE` and calls `iommu_probe_device()`
  internally. Because our notifier has a higher priority and runs first,
  the fwspec is already in place by the time the kernel's notifier
  probes.

The IOMMU's `map_pages` / `unmap_pages` / `attach_dev` callbacks are no-ops
or identity. **No real DMA path is provided**, because the fake PCI devices
themselves have no usable BARs and never DMA. The whole purpose is to make
the chain of kernel checks vfio-pci -> KubeVirt virt-handler -> libvirt ->
QEMU complete successfully.

## Scope and limitations

| Capability | Supported |
|---|---|
| vfio-pci binding to fake devices | yes |
| `/dev/vfio/<group_id>` device file appears | yes |
| KubeVirt virt-handler pre-start hook succeeds | yes |
| VMI reaches `Running` | yes |
| QEMU's VFIO ioctls succeed | yes |
| Guest OS sees a non-crashing PCI device | depends on fake-nvidia-pci BAR backing |
| Guest OS sees a *working* GPU | **no** (no real device behind the BARs) |
| Real DMA from a fake device to guest memory | **no** (no page tables backing IOVA) |

This module is strictly for testing **the KubeVirt + DRA PCI passthrough
lifecycle**. Do not load it on production hosts: it taints the kernel and
registers an IOMMU that does not actually isolate DMA.

## Requirements

- Linux kernel **6.0+** with:
  - `CONFIG_IOMMU_API=y`
  - `CONFIG_VFIO_PCI=y` or `=m` (loaded)
- `fake-nvidia-pci` module (either built and ready to load, or already
  loaded - either order works; the bus notifier and the
  "claim existing devices" walk together cover both load orders).

## Build

```bash
cd kubevirtci/cluster-up/cluster/kind-1.35-vgpu/fake-iommu

# In a container matching the running kernel:
docker run --rm -v $(pwd):/src:Z \
  quay.io/kubevirtci/bootstrap:v20251218-e7a7fc9 \
  bash -c 'dnf install -y kernel-devel >/dev/null 2>&1 && cd /src && \
           make KDIR=/usr/src/kernels/$(ls /usr/src/kernels/ | head -1) modules'

# Or just on the host:
make
```

## Load / unload (manual)

```bash
# Correct load order:
sudo insmod fake-iommu.ko target_domain=0xfaca
sudo insmod ../fake-nvidia-pci/fake-nvidia-pci.ko

# Correct unload order (reverse):
sudo rmmod fake-nvidia-pci
sudo rmmod fake-iommu
```

The `setup-fake-pci-host.sh` helper one directory up enforces this order.

## Verifying it works

After loading both modules:

```bash
# 1. The IOMMU itself shows up
ls /sys/class/iommu/
# expected: fake-iommu

# 2. Each fake PCI device now belongs to a real IOMMU group
ls /sys/bus/pci/devices/faca:00:00.0/iommu_group/devices/
# expected: faca:00:00.0

readlink /sys/bus/pci/devices/faca:00:00.0/iommu_group
# expected: ../../../kernel/iommu_groups/<N>

# 3. vfio-pci can now bind
echo 10de 1eb8 | sudo tee /sys/bus/pci/drivers/vfio-pci/new_id
lspci -ks faca:00:00.0
# expected: "Kernel driver in use: vfio-pci"

# 4. The vfio group character device exists
ls /dev/vfio/
# expected: a numeric group node plus the "vfio" container
```

## Module parameter

| Param | Default | Meaning |
|---|---|---|
| `target_domain` | `0xfaca` | PCI domain whose devices to claim. Must match `fake-nvidia-pci`'s `pci_domain`. |

## How it differs from the existing kernel options

| Approach | Works for our case? | Trade-off |
|---|---|---|
| `vfio.enable_unsafe_noiommu_mode=1` | partial: vfio-pci binds, but VFIO_IOMMU_MAP_DMA fails -> QEMU cannot map guest memory -> VM start still fails | taints kernel, intended only for devices with truly no DMA |
| `virtio-iommu` | no: it is a *consumer* driver expecting a virtio-iommu device on a hypervisor bus; we need to be the *provider* | requires VM-host coordination |
| **fake-iommu (this module)** | yes: vfio-pci binds, IOMMU map ioctls succeed, VMI reaches Running | taints kernel, no real DMA isolation; only safe on test hosts |

## How it works under the hood

```
fake_iommu_init()
  fwnode_create_software_node()              -> /dev/.../sw-node
  platform_device_register_simple()          -> /sys/devices/platform/fake_iommu.0
  iommu_device_sysfs_add(..., "fake-iommu")  -> /sys/class/iommu/fake-iommu
  bus_register_notifier(&pci_bus_type, nb)   -> hook PCI hotplug (priority 100)
  fake_iommu_seed_existing()                 -> set fwspec on already-present devs
  iommu_device_register(&dev, &ops, hwdev)
    -> kernel walks pci_bus_type via bus_iommu_probe()
    -> for every device with a matching fwspec, calls our probe_device ops
    -> /sys/bus/pci/devices/<bdf>/iommu_group symlink appears

<later, when fake-nvidia-pci adds new devices>
pci_bus -> BUS_NOTIFY_ADD_DEVICE
  (priority 100) fake_iommu_bus_notify() -> fake_iommu_set_fwspec()
                    iommu_fwspec_init(dev, fwnode, &ops)
  (priority 0)   kernel iommu_bus_notifier()
                    iommu_probe_device(dev)     [private API, kernel-internal]
                       -> iommu_ops_from_fwnode(fwspec->iommu_fwnode)
                       -> ops.probe_device(dev) returns &fake_iommu_dev
                       -> ops.device_group(dev) = generic_device_group()
                       -> /sys/bus/pci/devices/<bdf>/iommu_group appears
```

When `vfio-pci` is later asked to bind to one of these devices, its probe
calls `iommu_group_get(dev)` and gets the group set up above. Probe
succeeds. virt-handler proceeds. virt-launcher hands the group fd to QEMU.
QEMU constructs the device. The VM reaches `Running`.

## Files

| File | License |
|---|---|
| `fake-iommu.c` | GPL-2.0-only |
| `compat.h` | GPL-2.0-only |
| `Makefile` | GPL-2.0-only |
| `dkms.conf` | GPL-2.0-only |
| `README.md` | Apache-2.0 |

## See also

- [`../fake-nvidia-pci/`](../fake-nvidia-pci/) - the synthetic PCI bus this
  IOMMU exists to support.
- [`../fake-nvidia-vgpu/`](../fake-nvidia-vgpu/) - the mdev path, which does
  not need this IOMMU (mdev provides its own emulated VFIO).
- Linux IOMMU API: <https://docs.kernel.org/driver-api/iommu.html>
