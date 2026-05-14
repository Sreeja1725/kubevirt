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
 
#endif
