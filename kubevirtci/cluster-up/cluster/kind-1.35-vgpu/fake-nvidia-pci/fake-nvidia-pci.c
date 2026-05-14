// SPDX-License-Identifier: GPL-2.0
/*
 * Fake NVIDIA PCI device emulation for KubeVirt DRA testing
 *
 * This module registers a synthetic PCI host bridge in a dedicated PCI domain
 * (default 0xfaca) and populates it with N fake NVIDIA Tesla T4 devices. The
 * devices appear under /sys/bus/pci/devices/<domain>:00:XX.0/ with proper
 * vendor/device/class/subsystem fields, so they are discoverable by:
 *
 *   - lspci (with -D)
 *   - KubeVirt's PermittedHostDevices listing
 *   - A DRA driver that scans /sys/bus/pci/devices/ to publish a ResourceSlice
 *   - The pciBusID metadata path in pkg/virt-launcher/virtwrap/device/
 *     hostdevice/dra/gpu_hostdev.go
 *
 * Scope and limitations:
 *
 *   - BARs are advertised as "no resource" (size 0). The PCI core does not
 *     allocate iomem windows for these devices. Anything that tries to mmap
 *     a BAR (vfio-pci, QEMU passthrough) will fail. This is intentional: the
 *     host has no real IOMMU group for these devices and we cannot synthesize
 *     a usable DMA path. Use the companion fake-nvidia-vgpu (mdev) module to
 *     exercise full VM attach end-to-end.
 *   - All synthesized devices are identical Tesla T4s by default. Override
 *     vendor_id and device_id module parameters to emulate other NVIDIA SKUs
 *     (e.g. device_id=0x20b0 for A100).
 *   - Devices appear in their own PCI domain (default 0xfaca) to avoid
 *     colliding with the real PCI hierarchy at domain 0x0000. Requires a
 *     kernel built with CONFIG_PCI_DOMAINS=y (true for x86_64 / arm64).
 *     The private domain is set via bridge->domain_nr on kernels with
 *     CONFIG_PCI_DOMAINS_GENERIC=y, or via an attached struct pci_sysdata
 *     on x86 builds where GENERIC is off (Ubuntu's default). See compat.h.
 *
 * Hotplug emulation:
 *   /sys/class/fake_nvidia_pci/control/hotplug_control accepts "hide" / "show"
 *   to simulate device disappear / reappear, mirroring the mdev module's
 *   interface and enabling tests of virt-handler's hot-plug detection.
 */

#include <linux/init.h>
#include <linux/module.h>
#include <linux/kernel.h>
#include <linux/slab.h>
#include <linux/pci.h>
#include <linux/device.h>
#include <linux/mutex.h>
#include <linux/numa.h>
#include <linux/string.h>
#include <linux/version.h>

#include "compat.h"

/*
 * On x86 builds without CONFIG_PCI_DOMAINS_GENERIC the PCI domain is read
 * from ((struct pci_sysdata *)bus->sysdata)->domain rather than from
 * bridge->domain_nr. Include the arch header so we can stamp our own
 * domain into a struct pci_sysdata that we attach to the bridge.
 */
#if !defined(CONFIG_PCI_DOMAINS_GENERIC) && \
    (defined(CONFIG_X86) || defined(CONFIG_X86_64))
#include <asm/pci.h>
#define FAKE_PCI_USE_X86_SYSDATA 1
#endif

#define DRIVER_NAME             "fake_nvidia_pci"
#define DRIVER_VERSION          "1.0"
#define FAKE_PCI_CLASS_NAME     "fake_nvidia_pci"

/* Defaults emulate NVIDIA Tesla T4 */
#define NVIDIA_VENDOR_ID        0x10de
#define NVIDIA_T4_DEVICE_ID     0x1eb8
#define NVIDIA_T4_SUBSYS_ID     0x12a2

#define FAKE_PCI_DEFAULT_DOMAIN   0xfaca
#define FAKE_PCI_DEFAULT_DEVICES  4
#define FAKE_PCI_MAX_DEVICES      32

#define FAKE_PCI_CONFIG_SIZE      256

#define STORE_LE16(addr, val)   (*(__le16 *)(addr) = cpu_to_le16(val))
#define STORE_LE32(addr, val)   (*(__le32 *)(addr) = cpu_to_le32(val))

/* ---------- module parameters ---------- */

static unsigned int num_devices = FAKE_PCI_DEFAULT_DEVICES;
module_param(num_devices, uint, 0444);
MODULE_PARM_DESC(num_devices,
		 "Number of fake PCI devices to expose (1.."
		 __stringify(FAKE_PCI_MAX_DEVICES) ")");

static unsigned int pci_domain = FAKE_PCI_DEFAULT_DOMAIN;
module_param(pci_domain, uint, 0444);
MODULE_PARM_DESC(pci_domain,
		 "PCI domain number for the synthetic bridge (default 0xfaca)");

static unsigned int vendor_id = NVIDIA_VENDOR_ID;
module_param(vendor_id, uint, 0444);
MODULE_PARM_DESC(vendor_id, "PCI vendor ID to emulate (default 0x10de NVIDIA)");

static unsigned int device_id = NVIDIA_T4_DEVICE_ID;
module_param(device_id, uint, 0444);
MODULE_PARM_DESC(device_id,
		 "PCI device ID to emulate (default 0x1eb8 Tesla T4)");

static unsigned int subsys_id = NVIDIA_T4_SUBSYS_ID;
module_param(subsys_id, uint, 0444);
MODULE_PARM_DESC(subsys_id, "PCI subsystem ID to emulate (default 0x12a2)");

/* ---------- module state ---------- */

struct fake_pci_dev_state {
	u8 config[FAKE_PCI_CONFIG_SIZE];
};

static struct fake_pci_state {
	struct fake_pci_dev_state *devices; /* num_devices entries */

	struct class *control_class;
	struct device control_dev;
	bool control_registered;

	struct pci_host_bridge *bridge;
	struct pci_bus *bus;
	bool bus_present;

#ifdef FAKE_PCI_USE_X86_SYSDATA
	struct pci_sysdata sysdata;
#endif

	struct mutex lock;
} fpci;

/* Bus number window: a single bus, number 0, on our private domain. */
static struct resource fake_pci_busn_res = {
	.name  = "fake-pci-busn",
	.start = 0,
	.end   = 0,
	.flags = IORESOURCE_BUS,
};

/* ---------- config space synthesis ---------- */

static void fake_pci_init_config(struct fake_pci_dev_state *d)
{
	u8 *c = d->config;

	memset(c, 0, FAKE_PCI_CONFIG_SIZE);

	STORE_LE16(&c[PCI_VENDOR_ID], vendor_id);
	STORE_LE16(&c[PCI_DEVICE_ID], device_id);

	/* Command: memory space + bus master enabled */
	STORE_LE16(&c[PCI_COMMAND], PCI_COMMAND_MEMORY | PCI_COMMAND_MASTER);
	/* Status: capabilities list present */
	STORE_LE16(&c[PCI_STATUS], PCI_STATUS_CAP_LIST);

	c[PCI_REVISION_ID]  = 0xa1;
	c[PCI_CLASS_PROG]   = 0x00;
	/* 0x0302 = Display controller / 3D controller */
	STORE_LE16(&c[PCI_CLASS_DEVICE], 0x0302);

	c[PCI_CACHE_LINE_SIZE] = 0x10;
	c[PCI_LATENCY_TIMER]   = 0x00;
	c[PCI_HEADER_TYPE]     = PCI_HEADER_TYPE_NORMAL;

	/*
	 * Leave BAR0..BAR5 at 0. Our write callback enforces that BAR sizing
	 * (write 0xFFFFFFFF then read back) yields 0, meaning "BAR not
	 * implemented". The PCI core will not allocate iomem for these
	 * devices, which is what we want: no real backing memory.
	 */

	STORE_LE16(&c[PCI_SUBSYSTEM_VENDOR_ID], vendor_id);
	STORE_LE16(&c[PCI_SUBSYSTEM_ID], subsys_id);

	/* Capabilities pointer */
	c[PCI_CAPABILITY_LIST] = 0x60;

	c[PCI_INTERRUPT_LINE] = 0xff;
	c[PCI_INTERRUPT_PIN]  = 0x01;

	/* PM capability @ 0x60 -> next 0x68 */
	c[0x60] = PCI_CAP_ID_PM;
	c[0x61] = 0x68;
	STORE_LE16(&c[0x62], 0x0003);  /* PMC */
	STORE_LE16(&c[0x64], 0x0000);  /* PMCSR */

	/* MSI capability @ 0x68 -> next 0x78 */
	c[0x68] = PCI_CAP_ID_MSI;
	c[0x69] = 0x78;
	STORE_LE16(&c[0x6a], 0x0080);  /* 64-bit capable, disabled */

	/* PCI Express capability @ 0x78 -> end of list */
	c[0x78] = PCI_CAP_ID_EXP;
	c[0x79] = 0x00;
	STORE_LE16(&c[0x7a], 0x0002);
	STORE_LE32(&c[0x7c], 0x00000010);
}

/* ---------- pci_ops ---------- */

static int fake_pci_dev_index(unsigned int devfn)
{
	unsigned int slot = PCI_SLOT(devfn);
	unsigned int func = PCI_FUNC(devfn);

	if (func != 0)
		return -1;
	if (slot >= num_devices)
		return -1;
	return (int)slot;
}

static int fake_pci_read(struct pci_bus *bus, unsigned int devfn,
			 int where, int size, u32 *val)
{
	int idx;
	const u8 *cfg;

	/* Only bus 0 on our domain hosts devices */
	if (bus->number != 0) {
		*val = ~0U;
		return PCIBIOS_SUCCESSFUL;
	}

	idx = fake_pci_dev_index(devfn);
	if (idx < 0) {
		*val = ~0U;
		return PCIBIOS_SUCCESSFUL;
	}

	if (where < 0 || where + size > FAKE_PCI_CONFIG_SIZE) {
		*val = 0;
		return PCIBIOS_BAD_REGISTER_NUMBER;
	}

	cfg = fpci.devices[idx].config;
	switch (size) {
	case 1:
		*val = cfg[where];
		break;
	case 2:
		*val = le16_to_cpup((__le16 *)&cfg[where]);
		break;
	case 4:
		*val = le32_to_cpup((__le32 *)&cfg[where]);
		break;
	default:
		return PCIBIOS_BAD_REGISTER_NUMBER;
	}

	return PCIBIOS_SUCCESSFUL;
}

static int fake_pci_write(struct pci_bus *bus, unsigned int devfn,
			  int where, int size, u32 val)
{
	int idx;
	u8 *cfg;

	if (bus->number != 0)
		return PCIBIOS_SUCCESSFUL;

	idx = fake_pci_dev_index(devfn);
	if (idx < 0)
		return PCIBIOS_SUCCESSFUL;

	if (where < 0 || where + size > FAKE_PCI_CONFIG_SIZE)
		return PCIBIOS_BAD_REGISTER_NUMBER;

	cfg = fpci.devices[idx].config;

	/*
	 * Restrict writes to fields that real PCI devices honor. Everything
	 * else is silently dropped to keep our synthetic state stable.
	 *
	 * BAR sizing: writing 0xFFFFFFFF to a BAR is the standard "size me"
	 * probe. We always store 0, so the readback is 0 and the PCI core
	 * concludes the BAR is unimplemented. This avoids resource allocation
	 * for memory we cannot back.
	 */
	switch (where) {
	case PCI_COMMAND:
		if (size == 2)
			STORE_LE16(&cfg[where], (u16)val);
		break;

	case PCI_BASE_ADDRESS_0:
	case PCI_BASE_ADDRESS_1:
	case PCI_BASE_ADDRESS_2:
	case PCI_BASE_ADDRESS_3:
	case PCI_BASE_ADDRESS_4:
	case PCI_BASE_ADDRESS_5:
		if (size == 4)
			STORE_LE32(&cfg[where], 0);
		break;

	default:
		/* Drop other writes */
		break;
	}

	return PCIBIOS_SUCCESSFUL;
}

static struct pci_ops fake_pci_ops = {
	.read  = fake_pci_read,
	.write = fake_pci_write,
};

/* ---------- bus bring-up / tear-down ---------- */

static int fake_pci_bring_up(void)
{
	struct pci_host_bridge *bridge;
	unsigned int i;
	int ret;

	bridge = pci_alloc_host_bridge(0);
	if (!bridge)
		return -ENOMEM;

	bridge->ops      = &fake_pci_ops;
	bridge->busnr    = 0;
	bridge->dev.parent = &fpci.control_dev;

	/*
	 * Place our synthetic bus in its own PCI domain so the BDFs we
	 * synthesize do not collide with the real PCI hierarchy at domain
	 * 0x0000. Two code paths get us there, picked at build time by
	 * compat.h:
	 *
	 *   - CONFIG_PCI_DOMAINS_GENERIC=y: pci_register_host_bridge()
	 *     honors bridge->domain_nr directly.
	 *   - x86 with GENERIC=n (Ubuntu's default): pci_domain_nr(bus)
	 *     reads ((struct pci_sysdata *)bus->sysdata)->domain. We stamp
	 *     our domain into a struct pci_sysdata embedded in our module
	 *     state and hand a pointer to it via bridge->sysdata.
	 */
#ifdef FAKE_PCI_USE_X86_SYSDATA
	memset(&fpci.sysdata, 0, sizeof(fpci.sysdata));
	fpci.sysdata.domain = (int)pci_domain;
	fpci.sysdata.node   = NUMA_NO_NODE;
	bridge->sysdata     = &fpci.sysdata;
#else
	bridge->sysdata     = NULL;
	bridge->domain_nr   = (int)pci_domain;
#endif

	/* Reset list pointers so we can re-use the static resource */
	fake_pci_busn_res.parent  = NULL;
	fake_pci_busn_res.child   = NULL;
	fake_pci_busn_res.sibling = NULL;

	pci_add_resource(&bridge->windows, &fake_pci_busn_res);

	ret = pci_host_probe(bridge);
	if (ret) {
		pr_err("%s: pci_host_probe failed: %d\n", DRIVER_NAME, ret);
		pci_free_host_bridge(bridge);
		return ret;
	}

	fpci.bridge      = bridge;
	fpci.bus         = bridge->bus;
	fpci.bus_present = true;

	pr_info("%s: bridge up on domain 0x%x, %u device(s)\n",
		DRIVER_NAME, pci_domain, num_devices);
	for (i = 0; i < num_devices; i++)
		pr_info("%s:   %04x:00:%02x.0 [%04x:%04x]\n",
			DRIVER_NAME, pci_domain, i, vendor_id, device_id);

	return 0;
}

static void fake_pci_tear_down(void)
{
	if (!fpci.bus_present)
		return;

	pci_lock_rescan_remove();
	pci_stop_root_bus(fpci.bus);
	pci_remove_root_bus(fpci.bus);
	pci_unlock_rescan_remove();

	/*
	 * pci_remove_root_bus drops the bridge's reference via the device
	 * model; pci_host_bridge memory is freed when the last reference is
	 * released. Do not call pci_free_host_bridge here.
	 */
	fpci.bridge      = NULL;
	fpci.bus         = NULL;
	fpci.bus_present = false;

	pr_info("%s: bridge torn down\n", DRIVER_NAME);
}

/* ---------- hotplug_control sysfs ---------- */

static ssize_t hotplug_control_show(struct device *dev,
				    struct device_attribute *attr, char *buf)
{
	const char *state;

	mutex_lock(&fpci.lock);
	state = fpci.bus_present ? "visible" : "hidden";
	mutex_unlock(&fpci.lock);

	return sprintf(buf, "%s\n", state);
}

static ssize_t hotplug_control_store(struct device *dev,
				     struct device_attribute *attr,
				     const char *buf, size_t count)
{
	ssize_t ret = count;
	int err;

	mutex_lock(&fpci.lock);

	if (sysfs_streq(buf, "hide")) {
		if (fpci.bus_present) {
			pr_info("%s: hiding bridge\n", DRIVER_NAME);
			fake_pci_tear_down();
		}
	} else if (sysfs_streq(buf, "show")) {
		if (!fpci.bus_present) {
			pr_info("%s: showing bridge\n", DRIVER_NAME);
			err = fake_pci_bring_up();
			if (err)
				ret = err;
		}
	} else {
		pr_err("%s: invalid hotplug_control value, expected 'hide' or 'show'\n",
		       DRIVER_NAME);
		ret = -EINVAL;
	}

	mutex_unlock(&fpci.lock);
	return ret;
}

static DEVICE_ATTR_RW(hotplug_control);

/* ---------- control device lifecycle ---------- */

static void fake_pci_control_release(struct device *dev)
{
	/* All teardown happens explicitly in module exit. */
}

static int fake_pci_create_control_device(void)
{
	int ret;

	fpci.control_class = COMPAT_CLASS_CREATE(FAKE_PCI_CLASS_NAME);
	if (IS_ERR(fpci.control_class))
		return PTR_ERR(fpci.control_class);

	fpci.control_dev.class   = fpci.control_class;
	fpci.control_dev.release = fake_pci_control_release;
	dev_set_name(&fpci.control_dev, "control");

	ret = device_register(&fpci.control_dev);
	if (ret) {
		put_device(&fpci.control_dev);
		class_destroy(fpci.control_class);
		fpci.control_class = NULL;
		return ret;
	}

	ret = device_create_file(&fpci.control_dev, &dev_attr_hotplug_control);
	if (ret) {
		device_unregister(&fpci.control_dev);
		class_destroy(fpci.control_class);
		fpci.control_class = NULL;
		return ret;
	}

	fpci.control_registered = true;
	return 0;
}

static void fake_pci_destroy_control_device(void)
{
	if (!fpci.control_registered)
		return;
	device_remove_file(&fpci.control_dev, &dev_attr_hotplug_control);
	device_unregister(&fpci.control_dev);
	class_destroy(fpci.control_class);
	fpci.control_class      = NULL;
	fpci.control_registered = false;
}

/* ---------- module init / exit ---------- */

static int __init fake_pci_init(void)
{
	unsigned int i;
	int ret;

	pr_info("%s: initializing (vendor=%#x device=%#x num=%u domain=%#x)\n",
		DRIVER_NAME, vendor_id, device_id, num_devices, pci_domain);

	if (num_devices == 0 || num_devices > FAKE_PCI_MAX_DEVICES) {
		pr_err("%s: invalid num_devices %u, must be 1..%d\n",
		       DRIVER_NAME, num_devices, FAKE_PCI_MAX_DEVICES);
		return -EINVAL;
	}

	memset(&fpci, 0, sizeof(fpci));
	mutex_init(&fpci.lock);

	fpci.devices = kcalloc(num_devices, sizeof(*fpci.devices), GFP_KERNEL);
	if (!fpci.devices)
		return -ENOMEM;

	for (i = 0; i < num_devices; i++)
		fake_pci_init_config(&fpci.devices[i]);

	ret = fake_pci_create_control_device();
	if (ret)
		goto err_free;

	ret = fake_pci_bring_up();
	if (ret)
		goto err_control;

	pr_info("%s: ready; hotplug control at /sys/class/%s/control/hotplug_control\n",
		DRIVER_NAME, FAKE_PCI_CLASS_NAME);
	return 0;

err_control:
	fake_pci_destroy_control_device();
err_free:
	kfree(fpci.devices);
	fpci.devices = NULL;
	return ret;
}

static void __exit fake_pci_exit(void)
{
	mutex_lock(&fpci.lock);
	fake_pci_tear_down();
	mutex_unlock(&fpci.lock);

	fake_pci_destroy_control_device();

	kfree(fpci.devices);
	fpci.devices = NULL;

	pr_info("%s: unloaded\n", DRIVER_NAME);
}

module_init(fake_pci_init);
module_exit(fake_pci_exit);

MODULE_DESCRIPTION("Fake NVIDIA PCI device emulation for KubeVirt DRA testing");
MODULE_LICENSE("GPL v2");
MODULE_VERSION(DRIVER_VERSION);
MODULE_AUTHOR("KubeVirt Fake PCI Driver");
