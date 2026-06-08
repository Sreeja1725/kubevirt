/* SPDX-License-Identifier: GPL-2.0 */
/*
 * Kernel compatibility header for fake-iommu module.
 */

#ifndef _FAKE_IOMMU_COMPAT_H
#define _FAKE_IOMMU_COMPAT_H

#include <linux/version.h>

#if LINUX_VERSION_CODE < KERNEL_VERSION(6, 0, 0)
#error "fake-iommu requires Linux kernel 6.0 or later (default_domain_ops layout)"
#endif

#ifndef CONFIG_IOMMU_API
#error "fake-iommu requires CONFIG_IOMMU_API=y"
#endif

/*
 * iommu_fwspec_init() dropped the ops argument in 6.11; the core now
 * resolves ops via iommu_ops_from_fwnode(), which requires the IOMMU to
 * already be registered for that fwnode.
 */
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 11, 0)
#define fake_iommu_fwspec_init(dev, fwnode, ops) \
	iommu_fwspec_init((dev), (fwnode))
#else
#define fake_iommu_fwspec_init(dev, fwnode, ops) \
	iommu_fwspec_init((dev), (fwnode), (ops))
#endif

#endif /* _FAKE_IOMMU_COMPAT_H */
