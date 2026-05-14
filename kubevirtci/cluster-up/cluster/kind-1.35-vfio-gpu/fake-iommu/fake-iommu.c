// SPDX-License-Identifier: GPL-2.0
/*
 *
 * Companion to fake-pci. Provides a software-only IOMMU that claims PCI
 * devices on a specific (synthetic) PCI domain, so that vfio-pci can bind
 * to those devices and the KubeVirt VMI lifecycle reaches Running.
 *
 * Scope:
 *   - This IOMMU's map / unmap / iova_to_phys are no-ops or identity. There
 *     is no real DMA path: the fake devices have no usable BARs and never
 *     actually access guest memory.
 *   - The driver only claims devices on the PCI domain advertised via the
 *     target_domain module parameter (default 0xfaca, matching fake-pci's
 *     default). Real PCI devices on domain 0000 are left alone for
 *     intel-iommu / amd-iommu / etc. to handle.
 *
 * Load order:
 *   modprobe fake-iommu                # this module first
 *   modprobe fake-pci                  # then the synthetic PCI bus
 *
 * Reverse on unload. The setup-fake-pci-host.sh helper enforces this.
 */

 #include <linux/init.h>
 #include <linux/module.h>
 #include <linux/kernel.h>
 #include <linux/slab.h>
 #include <linux/iommu.h>
 #include <linux/pci.h>
 #include <linux/device.h>
 #include <linux/notifier.h>
 #include <linux/platform_device.h>
 #include <linux/property.h>
 #include <linux/sizes.h>
 
 #include "compat.h"
 
 #define DRIVER_NAME             "fake_iommu"
 #define DRIVER_VERSION          "1.0"
 
 #define DEFAULT_TARGET_DOMAIN   0xfaca
 
 /* ---------- module parameters ---------- */
 
 static unsigned int target_domain = DEFAULT_TARGET_DOMAIN;
module_param(target_domain, uint, 0444);
MODULE_PARM_DESC(target_domain,
         "PCI domain number whose devices this fake IOMMU should claim "
         "(default 0xfaca; must match fake-pci's pci_domain)");
 
 /* ---------- module state ---------- */
 
 static struct iommu_device     fake_iommu_dev;
 static struct fwnode_handle   *fake_iommu_fwnode;
 static struct platform_device *fake_iommu_pdev;
 static struct notifier_block   pci_bus_nb;
 static bool                    bus_nb_registered;
 static bool                    iommu_registered;
 
 /* ---------- iommu_domain_ops ---------- */
 
 struct fake_iommu_domain {
     struct iommu_domain domain;
 };
 
 static int fake_iommu_attach_dev(struct iommu_domain *domain,
                  struct device *dev)
 {
     /* No real backing: attach always succeeds. */
     return 0;
 }
 
 static int fake_iommu_map_pages(struct iommu_domain *domain,
                 unsigned long iova, phys_addr_t paddr,
                 size_t pgsize, size_t pgcount, int prot,
                 gfp_t gfp, size_t *mapped)
 {
     *mapped = pgsize * pgcount;
     return 0;
 }
 
 static size_t fake_iommu_unmap_pages(struct iommu_domain *domain,
                      unsigned long iova,
                      size_t pgsize, size_t pgcount,
                      struct iommu_iotlb_gather *gather)
 {
     return pgsize * pgcount;
 }
 
 static phys_addr_t fake_iommu_iova_to_phys(struct iommu_domain *domain,
                        dma_addr_t iova)
 {
     /* Identity mapping is harmless because no DMA occurs. */
     return (phys_addr_t)iova;
 }
 
 static void fake_iommu_domain_free(struct iommu_domain *domain)
 {
     kfree(container_of(domain, struct fake_iommu_domain, domain));
 }
 
 static const struct iommu_domain_ops fake_iommu_domain_ops = {
     .attach_dev    = fake_iommu_attach_dev,
     .map_pages     = fake_iommu_map_pages,
     .unmap_pages   = fake_iommu_unmap_pages,
     .iova_to_phys  = fake_iommu_iova_to_phys,
     .free          = fake_iommu_domain_free,
 };
 
/*
  * Static identity and blocked domains.
*/
 static void fake_iommu_static_domain_free(struct iommu_domain *domain)
 {
     /* no-op: identity / blocked domains are statically allocated */
 }
 
 static const struct iommu_domain_ops fake_iommu_static_domain_ops = {
     .attach_dev = fake_iommu_attach_dev,
     .free       = fake_iommu_static_domain_free,
 };
 
 static struct iommu_domain fake_iommu_identity_domain = {
     .type = IOMMU_DOMAIN_IDENTITY,
     .ops  = &fake_iommu_static_domain_ops,
 };
 
 static struct iommu_domain fake_iommu_blocked_domain = {
     .type = IOMMU_DOMAIN_BLOCKED,
     .ops  = &fake_iommu_static_domain_ops,
 };
 
 static struct iommu_domain *fake_iommu_domain_alloc(unsigned type)
 {
     struct fake_iommu_domain *d;
 
     /*
      * IDENTITY / BLOCKED requests are normally served by the static
      * domains above (the core checks ops->identity_domain /
      * ops->blocked_domain first). We still handle them here as a
      * fallback in case the caller bypasses the static-pointer
      * fast-path.
      */
     if (type == IOMMU_DOMAIN_IDENTITY)
         return &fake_iommu_identity_domain;
     if (type == IOMMU_DOMAIN_BLOCKED)
         return &fake_iommu_blocked_domain;
 
     if (type != IOMMU_DOMAIN_DMA && type != IOMMU_DOMAIN_UNMANAGED)
         return NULL;
 
     d = kzalloc(sizeof(*d), GFP_KERNEL);
     if (!d)
         return NULL;
 
     d->domain.geometry.aperture_start = 0;
     d->domain.geometry.aperture_end   = ~0ULL;
     d->domain.geometry.force_aperture = true;
     d->domain.pgsize_bitmap           = SZ_4K;
 
     return &d->domain;
 }
 
 /* ---------- iommu_ops ---------- */
 
 static bool fake_iommu_device_on_target_domain(struct device *dev)
 {
     struct pci_dev *pdev;
 
     if (!dev_is_pci(dev))
         return false;
     pdev = to_pci_dev(dev);
     return pci_domain_nr(pdev->bus) == (int)target_domain;
 }
 
 static struct iommu_device *fake_iommu_probe_device(struct device *dev)
 {
     if (!fake_iommu_device_on_target_domain(dev))
         return ERR_PTR(-ENODEV);
 
     dev_dbg(dev, "%s: probe_device claimed\n", DRIVER_NAME);
     return &fake_iommu_dev;
 }
 
 static void fake_iommu_release_device(struct device *dev)
 {
     dev_dbg(dev, "%s: release_device\n", DRIVER_NAME);
 }
 
 static struct iommu_group *fake_iommu_device_group(struct device *dev)
 {
     /*
      * generic_device_group() puts each device in its own IOMMU group,
      * which is what vfio-pci wants for single-device passthrough.
      */
     return generic_device_group(dev);
 }
 
 /*
  * vfio_register_group_dev() requires IOMMU_CAP_CACHE_COHERENCY (it sets
  * IOMMU_CACHE on every mapping unconditionally) and silently returns
  * -EINVAL from vfio-pci probe if the IOMMU does not advertise it. With no
  * .capable callback, device_iommu_capable() returns false for every cap, so
  * the bind quietly fails before any vfio-pci dev_dbg fires.
  */
 static bool fake_iommu_capable(struct device *dev, enum iommu_cap cap)
 {
     switch (cap) {
     case IOMMU_CAP_CACHE_COHERENCY:
         return true;
     default:
         return false;
     }
 }
 
 static const struct iommu_ops fake_iommu_ops = {
     .capable             = fake_iommu_capable,
     .domain_alloc        = fake_iommu_domain_alloc,
     .probe_device        = fake_iommu_probe_device,
     .release_device      = fake_iommu_release_device,
     .device_group        = fake_iommu_device_group,
     .pgsize_bitmap       = SZ_4K,
     .identity_domain     = &fake_iommu_identity_domain,
     .blocked_domain      = &fake_iommu_blocked_domain,
     .default_domain_ops  = &fake_iommu_domain_ops,
     .owner               = THIS_MODULE,
 };
 
 /* ---------- claiming devices via fwspec + PCI bus notifier ---------- */
 
 static int fake_iommu_set_fwspec(struct device *dev)
 {
     int ret;
 
     if (!fake_iommu_device_on_target_domain(dev))
         return 0;
 
     if (dev_iommu_fwspec_get(dev)) {
         dev_dbg(dev, "%s: device already has an iommu fwspec\n",
             DRIVER_NAME);
         return 0;
     }
 
     ret = iommu_fwspec_init(dev, fake_iommu_fwnode, &fake_iommu_ops);
     if (ret) {
         dev_err(dev, "%s: iommu_fwspec_init failed: %d\n",
             DRIVER_NAME, ret);
         return ret;
     }
 
     dev_info(dev, "%s: fwspec set; awaiting iommu probe\n", DRIVER_NAME);
     return 0;
 }
 
 static int fake_iommu_bus_notify(struct notifier_block *nb,
                  unsigned long action, void *data)
 {
     struct device *dev = data;
     if (action == BUS_NOTIFY_ADD_DEVICE)
         (void)fake_iommu_set_fwspec(dev);
 
     return NOTIFY_OK;
 }
 
 static void fake_iommu_seed_existing(void)
 {
     struct pci_dev *pdev = NULL;
 
    /*
     * fake-pci may have been loaded before us, in which case its
     * devices already exist and our bus notifier never fired for them.
     * Walk every PCI device and set fwspec on the ones on our target
     * domain so iommu_device_register()'s bus_iommu_probe() picks them
     * up.
     */
     for_each_pci_dev(pdev) {
         if (pci_domain_nr(pdev->bus) == (int)target_domain)
             (void)fake_iommu_set_fwspec(&pdev->dev);
     }
 }
 
 /* ---------- module init / exit ---------- */
 
 /*
  * fwnode_create_software_node() needs at least an empty property array. We
  * don't expose any properties; the fwnode exists solely so that
  * iommu_device_register() has something to bind to.
  */
 static const struct property_entry fake_iommu_props[] = {
     { }
 };
 
 static int __init fake_iommu_init(void)
 {
     int ret;
 
     pr_info("%s: initializing for PCI domain 0x%x\n",
         DRIVER_NAME, target_domain);
 
     /* 1. Software fwnode that the iommu_device will own. */
     fake_iommu_fwnode = fwnode_create_software_node(fake_iommu_props, NULL);
     if (IS_ERR(fake_iommu_fwnode)) {
         ret = PTR_ERR(fake_iommu_fwnode);
         fake_iommu_fwnode = NULL;
         pr_err("%s: fwnode_create_software_node failed: %d\n",
                DRIVER_NAME, ret);
         return ret;
     }
 
     /*
      * 2. A platform device acts as the hwdev parent for the iommu. The
      *    IOMMU subsystem expects a real struct device.
      */
     fake_iommu_pdev = platform_device_register_simple(DRIVER_NAME, -1,
                               NULL, 0);
     if (IS_ERR(fake_iommu_pdev)) {
         ret = PTR_ERR(fake_iommu_pdev);
         fake_iommu_pdev = NULL;
         pr_err("%s: platform_device_register_simple failed: %d\n",
                DRIVER_NAME, ret);
         goto err_fwnode;
     }
 
     /* 3. Sysfs entry under /sys/class/iommu/fake-iommu */
     ret = iommu_device_sysfs_add(&fake_iommu_dev, &fake_iommu_pdev->dev,
                      NULL, "fake-iommu");
     if (ret) {
         pr_err("%s: iommu_device_sysfs_add failed: %d\n",
                DRIVER_NAME, ret);
         goto err_pdev;
     }
 
     fake_iommu_dev.fwnode = fake_iommu_fwnode;
 
     /*
      * 4. Install our high-priority PCI bus notifier *before* registering
      *    the IOMMU. The kernel's own iommu bus notifier (priority 0) is
      *    installed inside iommu_device_register() and runs after ours,
      *    so by the time it probes a newly-added device, our notifier
      *    has already populated the fwspec.
      */
     pci_bus_nb.notifier_call = fake_iommu_bus_notify;
     pci_bus_nb.priority = 100;
     ret = bus_register_notifier(&pci_bus_type, &pci_bus_nb);
     if (ret) {
         pr_err("%s: bus_register_notifier(pci) failed: %d\n",
                DRIVER_NAME, ret);
         goto err_sysfs;
     }
     bus_nb_registered = true;
 
     /*
      * 5. Seed fwspec on fake PCI devices that were created before we
      *    loaded. iommu_device_register() will then probe them via
      *    bus_iommu_probe().
      */
     fake_iommu_seed_existing();
 
     /*
      * 6. Register with the IOMMU framework. This walks every device on
      *    pci_bus_type via bus_iommu_probe() and calls our
      *    fake_iommu_probe_device() ops callback for each one whose
      *    fwspec points at our fwnode.
      */
     ret = iommu_device_register(&fake_iommu_dev, &fake_iommu_ops,
                     &fake_iommu_pdev->dev);
     if (ret) {
         pr_err("%s: iommu_device_register failed: %d\n",
                DRIVER_NAME, ret);
         goto err_nb;
     }
     iommu_registered = true;
 
     pr_info("%s: ready (claiming PCI devices on domain 0x%x)\n",
         DRIVER_NAME, target_domain);
     return 0;
 
 err_nb:
     bus_unregister_notifier(&pci_bus_type, &pci_bus_nb);
     bus_nb_registered = false;
 err_sysfs:
     iommu_device_sysfs_remove(&fake_iommu_dev);
 err_pdev:
     platform_device_unregister(fake_iommu_pdev);
     fake_iommu_pdev = NULL;
 err_fwnode:
     fwnode_remove_software_node(fake_iommu_fwnode);
     fake_iommu_fwnode = NULL;
     return ret;
 }
 
 static void __exit fake_iommu_exit(void)
 {
     if (bus_nb_registered) {
         bus_unregister_notifier(&pci_bus_type, &pci_bus_nb);
         bus_nb_registered = false;
     }
     if (iommu_registered) {
         iommu_device_unregister(&fake_iommu_dev);
         iommu_registered = false;
     }
     iommu_device_sysfs_remove(&fake_iommu_dev);
     if (fake_iommu_pdev) {
         platform_device_unregister(fake_iommu_pdev);
         fake_iommu_pdev = NULL;
     }
     if (fake_iommu_fwnode) {
         fwnode_remove_software_node(fake_iommu_fwnode);
         fake_iommu_fwnode = NULL;
     }
     pr_info("%s: unloaded\n", DRIVER_NAME);
 }
 
 module_init(fake_iommu_init);
 module_exit(fake_iommu_exit);
 
 MODULE_DESCRIPTION("Example IOMMU companion to fake-pci for KubeVirt DRA testing");
 MODULE_LICENSE("GPL v2");
 MODULE_VERSION(DRIVER_VERSION);
 MODULE_AUTHOR("KubeVirt Fake IOMMU Driver");
 