/* SPDX-License-Identifier: GPL-2.0 */
/*
 * Kernel compatibility header for fake-iommu module.
 *
 * The Linux IOMMU framework went through significant restructuring in 6.0:
 *   - iommu_ops::default_domain_ops sub-struct was introduced
 *   - per-domain map/attach/iova_to_phys callbacks moved into iommu_domain_ops
 *   - iommu_ops::capable() callback signature changed
 *
 * Rather than carry shim layers for the pre-6.0 layout, this module hard-
 * targets 6.0+. Fedora 38+, RHEL 10, Ubuntu 22.10+/24.04, Debian bookworm
 * backports kernels all satisfy this. Older distros need a kernel upgrade
 * or stick to the device-plugin / mdev path for vGPU testing.
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

#endif /* _FAKE_IOMMU_COMPAT_H */
