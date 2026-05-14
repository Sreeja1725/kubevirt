/* SPDX-License-Identifier: GPL-2.0 */
/*
 * Kernel compatibility header for fake-nvidia-pci module.
 *
 * The module is built against the running kernel and targets 5.16+ to match
 * the companion fake-nvidia-vgpu mdev module. The PCI host bridge APIs used
 * here (pci_alloc_host_bridge, pci_host_probe, pci_remove_root_bus) have
 * been stable since well before 5.16, so only a couple of small shims are
 * needed.
 */

#ifndef _FAKE_NVIDIA_PCI_COMPAT_H
#define _FAKE_NVIDIA_PCI_COMPAT_H

#include <linux/version.h>

#if LINUX_VERSION_CODE < KERNEL_VERSION(5, 16, 0)
#error "This module requires kernel 5.16 or later"
#endif

#ifndef CONFIG_PCI_DOMAINS
#error "This module requires CONFIG_PCI_DOMAINS=y (private PCI domain needed to avoid host bus collision)"
#endif

/*
 * CONFIG_PCI_DOMAINS_GENERIC=y is what actually causes the kernel to honor
 * pci_host_bridge->domain_nr at register time. Without it the field is
 * ignored and the synthetic bridge would land on domain 0, colliding with
 * the real host PCI hierarchy. Mainline x86_64 and arm64 distro kernels
 * (Fedora 39+, RHEL 9, Ubuntu 22.04+, Debian bookworm+) enable this; if
 * yours doesn't, rebuild the kernel with this option.
 */
#ifndef CONFIG_PCI_DOMAINS_GENERIC
#error "This module requires CONFIG_PCI_DOMAINS_GENERIC=y (otherwise bridge->domain_nr is ignored and our private domain collides with bus 0000)"
#endif

/*
 * class_create() signature changed in 6.4.
 *   Before 6.4: class_create(owner, name)
 *   6.4+:       class_create(name)
 */
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 4, 0)
#define COMPAT_CLASS_CREATE(name) class_create(name)
#else
#define COMPAT_CLASS_CREATE(name) class_create(THIS_MODULE, name)
#endif

#endif /* _FAKE_NVIDIA_PCI_COMPAT_H */
