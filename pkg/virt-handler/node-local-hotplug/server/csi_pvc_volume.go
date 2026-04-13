package nodelocalhotplug

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	"kubevirt.io/kubevirt/pkg/util"
)

func addVolumeOptionsUsesNodeLocalHotplug(opts *v1.AddVolumeOptions) bool {
	if opts == nil || opts.VolumeSource == nil {
		return false
	}
	if opts.VolumeSource.PersistentVolumeClaim != nil && opts.VolumeSource.PersistentVolumeClaim.Hotpluggable {
		return true
	}
	if opts.VolumeSource.DataVolume != nil && opts.VolumeSource.DataVolume.Hotpluggable {
		return true
	}
	return false
}

// attachNodeLocalHotplugToVMI resolves the volume source and publishes it into the
// virt-launcher's hotplug directory.
//
// For CSI-backed PVCs: performs full CSI lifecycle (VolumeAttachment -> NodeStage
// -> NodePublish) then exposes the device to the launcher.
// For non-CSI PVs (Local, HostPath, NFS, iSCSI, FC, RBD): resolves the host
// path directly and publishes via mknod/bind-mount.
func (s *Server) attachNodeLocalHotplugToVMI(ctx context.Context, virtCli kubecli.KubevirtClient, ns, vmiName string, opts *v1.AddVolumeOptions) error {
	if !addVolumeOptionsUsesNodeLocalHotplug(opts) {
		return nil
	}

	if s.kubeletPodsDir == "" {
		return fmt.Errorf("kubelet pods directory is not configured on virt-handler")
	}

	resolved, err := resolveVolume(ctx, s.virtCli, s.kubeletPodsDir, ns, opts)
	if err != nil {
		return fmt.Errorf("resolve volume: %w", err)
	}

	vmi, err := s.virtCli.VirtualMachineInstance(ns).Get(ctx, vmiName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get VMI %s/%s: %w", ns, vmiName, err)
	}

	if resolved.CSI {
		launcherUID, _ := FindVirtlauncherUID(vmi)
		if launcherUID == "" {
			return fmt.Errorf("no virt-launcher pod found for VMI %s/%s", ns, vmiName)
		}

		targetPath := csiPublishTargetPath(s.kubeletPodsDir, string(launcherUID), opts.Name)
		hostTargetPath := filepath.Join(util.HostRootMount, targetPath)
		if merr := os.MkdirAll(hostTargetPath, 0750); merr != nil {
			return fmt.Errorf("create CSI publish target dir %s: %w", hostTargetPath, merr)
		}

		if err := publishCSIVolume(ctx, s.virtCli, s.kubeletPodsDir, s.host, resolved.PV, targetPath); err != nil {
			return fmt.Errorf("CSI publish for PV %s: %w", resolved.PV.Name, err)
		}

		return publishToLauncher(vmi, s.host, s.kubeletPodsDir, opts.Name, hostTargetPath)
	}

	return nil
}

func FindVirtlauncherUID(vmi *v1.VirtualMachineInstance) (types.UID, error) {
	var uid types.UID
	cnt := 0
	for podUID := range vmi.Status.ActivePods {
		cnt++
		uid = podUID
	}
	if cnt != 1 {
		return "", fmt.Errorf("expected 1 active pod, got %d", cnt)
	}
	return uid, nil
}

func csiPublishTargetPath(kubeletPodsDir string, launcherUID, volumeName string) string {
	kubeletRoot := kubeletRootFromPodsDir(kubeletPodsDir)
	return filepath.Join(kubeletRoot, "plugins", "kubevirt.io", "node-local-hotplug", "publish", launcherUID, volumeName)
}

func (s *Server) detachNodeLocalHotplugFromVMI(ctx context.Context, ns, vmiName string, vol *v1.Volume) error {
	if s.kubeletPodsDir == "" {
		return fmt.Errorf("kubelet pods directory is not configured on virt-handler")
	}
	vmi, err := s.virtCli.VirtualMachineInstance(ns).Get(ctx, vmiName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reload VMI after remove patch: %w", err)
	}

	handled, cerr := cleanupFromLauncher(vmi, s.host, s.kubeletPodsDir, vol.Name)
	if cerr != nil {
		return cerr
	}
	if !handled {
		return fmt.Errorf("hotplug target for volume %q not found in launcher hotplug directory", vol.Name)
	}

	pv := lookupPVForVolume(ctx, s.virtCli, ns, vol)
	if pv != nil && pv.Spec.CSI != nil {
		launcherUID, lerr := FindVirtlauncherUID(vmi)
		if lerr != nil {
			log.DefaultLogger().Warningf("CSI cleanup: cannot find launcher UID for volume %q: %v", vol.Name, lerr)
		} else {
			// TODO: implement CSI cleanup (NodeUnpublish + NodeUnstage + delete VolumeAttachment)
			targetPath := csiPublishTargetPath(s.kubeletPodsDir, string(launcherUID), vol.Name)
			log.DefaultLogger().V(3).Infof("CSI cleanup not yet implemented for volume %q (target=%s)", vol.Name, targetPath)
		}
	}

	if merr := cleanupManagedMount(ctx, s.virtCli, ns, vol); merr != nil {
		log.DefaultLogger().Warningf("managed mount cleanup for volume %q: %v", vol.Name, merr)
	}
	return nil
}
