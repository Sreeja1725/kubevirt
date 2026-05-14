# Fake NVIDIA PCI Kernel Emulation

This kernel module exposes **synthetic NVIDIA PCI devices** (default: Tesla T4)
under `/sys/bus/pci/devices/` so that the KubeVirt DRA `pciBusID` path can be
exercised in the kind-vgpu cluster **without real GPU hardware**.

It is the PCI-passthrough analogue of the companion `fake-nvidia-vgpu` mdev
module. The two modules are independent and can be loaded together.

## What you get

After loading the module with defaults, `lspci -D -nn -d 10de:` shows:

```
faca:00:00.0 3D controller [0302]: NVIDIA Corporation TU104GL [Tesla T4] [10de:1eb8]
faca:00:01.0 3D controller [0302]: NVIDIA Corporation TU104GL [Tesla T4] [10de:1eb8]
faca:00:02.0 3D controller [0302]: NVIDIA Corporation TU104GL [Tesla T4] [10de:1eb8]
faca:00:03.0 3D controller [0302]: NVIDIA Corporation TU104GL [Tesla T4] [10de:1eb8]
```

Each device exposes the full set of standard PCI sysfs attributes
(`vendor`, `device`, `class`, `subsystem_vendor`, `subsystem_device`,
`config`, `irq`, etc.) so userspace tooling treats them as ordinary PCI
devices.

The synthetic devices live in their own **private PCI domain** (default
`0xfaca`) so they do not collide with the real PCI hierarchy at domain
`0000`.

## Scope and limitations

Capabilities depend on whether the companion `fake-iommu` module is also
loaded. `setup-fake-pci-host.sh` loads both by default (`FAKE_IOMMU=true`).

| Capability | This module alone | + fake-iommu |
|---|---|---|
| `/sys/bus/pci/devices/<bdf>/` entries | yes | yes |
| `lspci -D` listing | yes | yes |
| DRA driver discovery (scan + ResourceSlice publish) | yes | yes |
| KEP-5304 metadata round-trip via `GetPCIAddressForClaim` | yes | yes |
| Building libvirt domXML in virt-launcher | yes | yes |
| `iommu_group` symlink under `/sys/bus/pci/devices/<bdf>/` | no | **yes** |
| `vfio-pci` binding to the fake devices | no | **yes** |
| `/dev/vfio/<group>` device files | no | **yes** |
| virt-handler's pre-start hook succeeds | no | **yes** |
| **VMI reaches `Running`** | **no** | **yes** |
| Guest sees a non-crashing PCI device | no | requires fake BAR backing (not implemented) |
| Guest has a working GPU | no | no (no real hardware behind the BARs) |
| Hot-plug emulation via `hotplug_control` | yes | yes |

For "VMI reaches Running" tests, load both modules (the default).
For pure discovery / claim / metadata tests, `FAKE_IOMMU=false` is enough.
For full VM-attach with a guest that uses the device, use the
`fake-nvidia-vgpu` (mdev) module instead - mdev provides its own emulated
VFIO interface.

## Requirements

- Linux kernel **5.16+** built with `CONFIG_PCI_DOMAINS=y` **and**
  `CONFIG_PCI_DOMAINS_GENERIC=y` (true for Fedora 39+, RHEL 9, Ubuntu 22.04+,
  Debian bookworm+ on x86_64 / arm64; the build will hard-error otherwise).
  Verify with `zgrep PCI_DOMAINS /proc/config.gz` or
  `grep PCI_DOMAINS /boot/config-$(uname -r)`.
- Kernel headers matching the running kernel
- `make`, `gcc`, root privileges to insmod

## Build

```bash
cd kubevirtci/cluster-up/cluster/kind-1.35-vgpu/fake-nvidia-pci

# Using container (matches kernel of CI hosts):
docker run --rm \
  -v $(pwd):/src:Z \
  quay.io/kubevirtci/bootstrap:v20251218-e7a7fc9 \
  bash -c 'dnf install -y kernel-devel >/dev/null 2>&1 && cd /src && \
           make KDIR=/usr/src/kernels/$(ls /usr/src/kernels/ | head -1) modules'

# Or just on the host:
make
```

## Load / unload

```bash
cd kubevirtci/cluster-up/cluster/kind-1.35-vgpu

# Load with defaults (4 fake T4 devices in domain 0xfaca):
sudo ./setup-fake-pci-host.sh setup

# Inspect:
sudo ./setup-fake-pci-host.sh status
lspci -D -nn -d 10de:

# Unload:
sudo ./setup-fake-pci-host.sh cleanup
```

### Environment variables (passed as module parameters)

| Variable | Default | Module param |
|---|---|---|
| `FAKE_PCI_DEVICES` | `4` | `num_devices` (1..32) |
| `FAKE_PCI_DOMAIN` | `0xfaca` | `pci_domain` |
| `FAKE_PCI_VENDOR_ID` | `0x10de` | `vendor_id` |
| `FAKE_PCI_DEVICE_ID` | `0x1eb8` | `device_id` (Tesla T4) |

Examples:

```bash
# 8 fake T4 devices:
FAKE_PCI_DEVICES=8 sudo ./setup-fake-pci-host.sh setup

# 2 fake A100 devices in a different domain (avoid collision if 0xfaca is taken):
FAKE_PCI_DEVICES=2 FAKE_PCI_DEVICE_ID=0x20b0 FAKE_PCI_DOMAIN=0xfada \
  sudo ./setup-fake-pci-host.sh setup
```

## Hot-plug emulation

```bash
sudo ./setup-fake-pci-host.sh hide   # tear down the synthetic bus
sudo ./setup-fake-pci-host.sh show   # re-create it

# Or directly:
echo hide > /sys/class/fake_nvidia_pci/control/hotplug_control
echo show > /sys/class/fake_nvidia_pci/control/hotplug_control
cat /sys/class/fake_nvidia_pci/control/hotplug_control
```

## How it works

1. The module allocates a `pci_host_bridge` via `pci_alloc_host_bridge()`,
   sets its `ops` to the module's `pci_ops.read` / `pci_ops.write`
   callbacks, and assigns `domain_nr = pci_domain`.
2. `pci_host_probe()` triggers a normal PCI scan on this new bridge. Our
   read callback synthesizes Tesla T4 config space for slots
   `0 .. num_devices-1` and returns `0xffff` (no device) for all other
   slots and functions. The kernel creates `pci_dev` objects and the
   standard sysfs entries.
3. BARs are reported as 0. When the PCI core probes BAR sizes (writing
   `0xFFFFFFFF` then reading back), our write callback stores 0 and the
   readback yields 0, which the core interprets as "BAR not implemented".
   No iomem windows are allocated.
4. The hot-plug control device under `/sys/class/fake_nvidia_pci/control/`
   exposes a `hotplug_control` file that tears down (`pci_stop_root_bus` +
   `pci_remove_root_bus`) or re-creates the synthetic bridge.

## How a DRA driver consumes this

A DRA driver running in the kind node:

1. Scans `/sys/bus/pci/devices/` looking for vendor `0x10de`. Picks up
   the entries on bus `faca:` (or whichever domain you configured).
2. Publishes a `ResourceSlice` advertising each fake BDF as a DRA device
   with attributes such as `resource.kubernetes.io/pciBusID = faca:00:00.0`
   and `nvidia.com/model = "Tesla T4"`.
3. On `NodePrepareResources`, writes the KEP-5304 metadata file at
   `/var/run/dra-device-attributes/<claimName>/<requestName>/<driver>-metadata.json`
   with `Attributes["resource.kubernetes.io/pciBusID"].StringValue = <bdf>`.
4. KubeVirt's virt-launcher reads that file via
   `pkg/dra/utils.go::GetPCIAddressForClaim` and builds the libvirt
   `<hostdev type='pci'>` block via
   `pkg/virt-launcher/virtwrap/device/hostdevice/dra/gpu_hostdev.go`.

The VM start will fail at the QEMU `vfio-pci` attach step (no IOMMU
backing) - that is the boundary at which the mdev path takes over.

## Files

| File | License | Notes |
|---|---|---|
| `fake-nvidia-pci.c` | GPL-2.0-only | Kernel module |
| `compat.h` | GPL-2.0-only | Kernel version shims |
| `Makefile` | GPL-2.0-only | kbuild |
| `dkms.conf` | GPL-2.0-only | DKMS install descriptor |
| `README.md` | Apache-2.0 | This file |
| `../fake-iommu/` | GPL-2.0-only | Companion no-op IOMMU; required for `vfio-pci` binding |
| `../setup-fake-pci-host.sh` | Apache-2.0 | Host-level helper (loads both modules in the right order) |

## References

- Companion IOMMU module (this unlocks `vfio-pci` + `Running` VMIs):
  [`../fake-iommu/README.md`](../fake-iommu/README.md)
- Companion mdev module (for full VM-attach with a working guest device):
  [`../fake-nvidia-vgpu/README.md`](../fake-nvidia-vgpu/README.md)
- DRA consumer code: [`pkg/virt-launcher/virtwrap/device/hostdevice/dra/gpu_hostdev.go`](../../../../../pkg/virt-launcher/virtwrap/device/hostdevice/dra/gpu_hostdev.go)
- DRA metadata reader: [`pkg/dra/utils.go`](../../../../../pkg/dra/utils.go)
- Linux PCI host bridge API: <https://docs.kernel.org/PCI/index.html>
