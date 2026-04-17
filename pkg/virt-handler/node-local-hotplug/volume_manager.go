package nodelocalhotplug

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/devices"
	"golang.org/x/sys/unix"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	hotplugdisk "kubevirt.io/kubevirt/pkg/hotplug-disk"

	diskutils "kubevirt.io/kubevirt/pkg/ephemeral-disk-utils"
	"kubevirt.io/kubevirt/pkg/safepath"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/virt-handler/cgroup"
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
	if opts.VolumeSource.CustomVolume != nil {
		return true
	}
	return false
}

// attachNodeLocalHotplugToVMI resolves the volume source and publishes it into the
// virt-launcher's hotplug directory.
//
// For CSI volumes: performs ControllerPublish → NodeStage → NodePublish via
// direct CSI RPCs, then bind-mounts the result into the launcher.
// For non-CSI volumes (Local, HostPath, NFS, iSCSI, FC, RBD): publishes
// the resolved host path directly via mknod/bind-mount.
func (s *Server) attachNodeLocalHotplugToVMI(ctx context.Context, virtCli kubecli.KubevirtClient, ns, vmiName string, opts *v1.AddVolumeOptions) error {
	if !addVolumeOptionsUsesNodeLocalHotplug(opts) {
		return nil
	}

	if s.kubeletPodsDir == "" {
		return fmt.Errorf("kubelet pods directory is not configured on virt-handler")
	}

	if opts.VolumeSource.CustomVolume != nil {
		return s.attachCustomVolume(ctx, ns, vmiName, opts)
	}

	resolved, err := resolveVolume(ctx, s.virtCli, s.kubeletPodsDir, ns, opts, s.host)
	if err != nil {
		return fmt.Errorf("resolve volume: %w", err)
	}

	vmi, err := s.virtCli.VirtualMachineInstance(ns).Get(ctx, vmiName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get VMI %s/%s: %w", ns, vmiName, err)
	}

	if resolved.CSIInfo != nil {
		launcherUID, _ := FindVirtlauncherUID(vmi)
		if launcherUID == "" {
			return fmt.Errorf("no virt-launcher pod found for VMI %s/%s", ns, vmiName)
		}

		targetPath := csiPublishTargetPath(s.kubeletPodsDir, string(launcherUID), opts.Name)
		hostTargetPath := filepath.Join(util.HostRootMount, targetPath)
		if merr := os.MkdirAll(hostTargetPath, 0750); merr != nil {
			return fmt.Errorf("create CSI publish target dir %s: %w", hostTargetPath, merr)
		}

		directPublished, err := publishCSIVolume(ctx, s.virtCli, s.kubeletPodsDir, s.host, resolved.CSIInfo, targetPath, s.hotplugConfig)
		if err != nil {
			return fmt.Errorf("CSI publish for volume %s: %w", resolved.CSIInfo.VolumeHandle, err)
		}

		if !directPublished {
			socketPath := csiNodePluginSocketPath(s.kubeletPodsDir, resolved.CSIInfo.Driver)
			return fmt.Errorf(
				"CSI node plugin socket not found at %s for driver %q (node-local hotplug cannot defer to attachment pod after marking volume NodeLocal); ensure the CSI driver is installed on this node",
				socketPath, resolved.CSIInfo.Driver)
		}

		return publishToLauncher(vmi, s.host, s.kubeletPodsDir, opts.Name, hostTargetPath)
	}

	if resolved.HostPath != "" {
		return publishToLauncher(vmi, s.host, s.kubeletPodsDir, opts.Name, resolved.HostPath)
	}

	return fmt.Errorf("volume resolved but neither CSI info nor host path available")
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

func targetPathFromKubeletPodsDir(kubeletPodsDir string, uid types.UID, volumeName string) string {
	path := hotplugdisk.TargetPodBasePath(kubeletPodsDir, uid)
	return filepath.Join(path, volumeName)
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

	if vol.CustomVolume != nil {
		return s.detachCustomVolume(ctx, vmi, vmiName, vol)
	}

	pv := lookupPVForVolume(ctx, s.virtCli, ns, vol)
	if pv != nil && pv.Spec.CSI != nil {
		launcherUID, lerr := FindVirtlauncherUID(vmi)
		if lerr != nil {
			return fmt.Errorf("CSI cleanup for volume %q: launcher pod: %w", vol.Name, lerr)
		}
		socketPath := csiNodePluginSocketPath(s.kubeletPodsDir, pv.Spec.CSI.Driver)
		if socketPath == "" {
			return fmt.Errorf("csi socket path is not configured for volume %q", vol.Name)
		}
		info, ierr := extractCSIVolumeInfo(ctx, s.virtCli, pv)
		if ierr != nil {
			return fmt.Errorf("CSI cleanup for volume %q: %w", vol.Name, ierr)
		}
		targetPath := csiPublishTargetPath(s.kubeletPodsDir, string(launcherUID), vol.Name)
		if err := unpublishCSIVolume(ctx, s.virtCli, s.kubeletPodsDir, s.host, info.VolumeHandle, targetPath, socketPath); err != nil {
			return fmt.Errorf("CSI cleanup for volume %q: %w", vol.Name, err)
		}
	}

	if merr := cleanupManagedMount(ctx, s.virtCli, ns, vol); merr != nil {
		log.DefaultLogger().Warningf("managed mount cleanup for volume %q: %v", vol.Name, merr)
	}
	return nil
}

// detachCustomVolume cleans up resources for a custom volume:
//   - persistentRegional: CSI unpublish (NodeUnpublish, NodeUnstage, ControllerUnpublish)
//   - ephemeral: delete the qcow2 image file from the managed directory
func (s *Server) detachCustomVolume(ctx context.Context, vmi *v1.VirtualMachineInstance, vmiName string, vol *v1.Volume) error {
	cv := vol.CustomVolume

	if pr := cv.PersistentRegional; pr != nil {
		resolvedHandle := makeHandle(pr.Handle, pr.Cluster, vmi.Namespace)
		return s.cleanupPersistentRegional(ctx, vmi, vmiName, vol.Name, resolvedHandle)
	}

	if cv.EphemeralLocal != nil {
		return s.cleanupEphemeralVolume(ctx, vmi, vmiName, vol.Name)
	}

	return fmt.Errorf("customVolume %q has neither persistentRegional nor ephemeral set", vol.Name)
}

func (s *Server) cleanupPersistentRegional(ctx context.Context, vmi *v1.VirtualMachineInstance, vmiName string, volumeName string, handle string) error {
	launcherUID, err := FindVirtlauncherUID(vmi)
	if err != nil {
		return fmt.Errorf("CSI cleanup for persistentRegional volume %q: %w", volumeName, err)
	}
	socketPath := s.hotplugConfig.PersistentRegional.CSISocketPath
	targetPath := csiPublishTargetPath(s.kubeletPodsDir, string(launcherUID), volumeName)
	if err := unpublishCSIVolume(ctx, s.virtCli, s.kubeletPodsDir, s.host, handle, targetPath, socketPath); err != nil {
		return fmt.Errorf("CSI cleanup for persistentRegional volume %q: %w", volumeName, err)
	}
	return nil
}

func (s *Server) cleanupEphemeralVolume(ctx context.Context, vmi *v1.VirtualMachineInstance, vmiName string, volumeName string) error {
	launcherUID, err := FindVirtlauncherUID(vmi)
	if err != nil {
		return fmt.Errorf("cleanup ephemeral volume %q: %w", volumeName, err)
	}

	socketPath := s.hotplugConfig.Ephemeral.CSISocketPath
	baseStagingPath := s.hotplugConfig.Ephemeral.StagingPath

	csiVolumeID, err := readCSIVolumeID(baseStagingPath, string(vmi.UID), volumeName)
	if err != nil {
		return fmt.Errorf("cleanup ephemeral volume %q: %w", volumeName, err)
	}

	targetPath := csiPublishTargetPath(s.kubeletPodsDir, string(launcherUID), volumeName)
	stagingPath := ephemeralStagingPath(baseStagingPath, string(vmi.UID), volumeName)

	if err := csiNodeUnpublish(ctx, socketPath, csiVolumeID, targetPath); err != nil {
		return fmt.Errorf("cleanup ephemeral volume %q: NodeUnpublish: %w", volumeName, err)
	}

	if err := ephemeralNodeUnstage(ctx, socketPath, csiVolumeID, stagingPath); err != nil {
		return fmt.Errorf("cleanup ephemeral volume %q: NodeUnstage: %w", volumeName, err)
	}

	if err := s.removeVolumeByID(ctx, csiVolumeID, socketPath); err != nil {
		return fmt.Errorf("cleanup ephemeral volume %q: DeleteVolume: %w", volumeName, err)
	}

	return nil
}

// attachCustomVolume handles attach for CustomVolumeSource:
//   - persistentRegional: builds csiVolumeInfo directly from the handle/driver,
//     then uses publishCSIVolume + publishToLauncher (same flow as PVC-backed CSI).
//   - ephemeral: creates a qcow2 image file on local storage using qemu-img,
//     then publishes the image file into the launcher hotplug directory.
func (s *Server) attachCustomVolume(ctx context.Context, ns, vmiName string, opts *v1.AddVolumeOptions) error {
	cv := opts.VolumeSource.CustomVolume

	vmi, err := s.virtCli.VirtualMachineInstance(ns).Get(ctx, vmiName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get VMI %s/%s: %w", ns, vmiName, err)
	}

	launcherUID, err := FindVirtlauncherUID(vmi)
	if err != nil {
		return fmt.Errorf("no virt-launcher pod found for VMI %s/%s: %w", vmi.Namespace, vmi.Name, err)
	}

	if pr := cv.PersistentRegional; pr != nil {
		return s.attachPersistentRegional(ctx, vmi, opts.Name, pr, launcherUID, !pr.Unencrypted)
	}
	if eph := cv.EphemeralLocal; eph != nil {
		return s.attachEphemeral(ctx, vmi, ns, vmiName, opts.Name, launcherUID, eph)
	}
	return fmt.Errorf("customVolume has neither persistentRegional nor ephemeral set")
}

func (s *Server) attachPersistentRegional(ctx context.Context, vmi *v1.VirtualMachineInstance, volumeName string, pr *v1.PersistentRegionalVolumeSource, launcherUID types.UID, encrypted bool) error {
	resolvedHandle := makeHandle(pr.Handle, pr.Cluster, vmi.Namespace)
	log.DefaultLogger().V(3).Infof("Resolved handle %q -> %q for VMI %s/%s volume %s", pr.Handle, resolvedHandle, vmi.Namespace, vmi.Name, volumeName)

	info := &csiVolumeInfo{
		VolumeHandle: resolvedHandle,
		VolumeMode:   k8sv1.PersistentVolumeBlock,
	}

	if err := csiControllerPublish(ctx, s.host, info, s.hotplugConfig); err != nil {
		return fmt.Errorf("CSI controller publish for persistentRegional volume %s: %w", resolvedHandle, err)
	}

	csiTargetPath := csiPublishTargetPath(s.kubeletPodsDir, string(launcherUID), volumeName)
	hostCSITargetPath := filepath.Join(util.HostRootMount, csiTargetPath)

	// Block volumes: CSI expects the target to be a file (not a directory)
	// so the driver can bind-mount the block device onto it.
	hostCSITargetDir := filepath.Dir(hostCSITargetPath)
	if err := os.MkdirAll(hostCSITargetDir, 0750); err != nil {
		return fmt.Errorf("create CSI publish target parent dir: %w", err)
	}
	if info.VolumeMode == k8sv1.PersistentVolumeBlock {
		f, err := os.OpenFile(hostCSITargetPath, os.O_CREATE|os.O_RDONLY, 0640)
		if err != nil {
			return fmt.Errorf("create CSI publish target file for block volume: %w", err)
		}
		f.Close()
	} else {
		if err := os.MkdirAll(hostCSITargetPath, 0750); err != nil {
			return fmt.Errorf("create CSI publish target dir: %w", err)
		}
	}

	socketPath := s.hotplugConfig.PersistentRegional.CSISocketPath

	if err := csiNodeStage(ctx, s.kubeletPodsDir, info, volumeName, encrypted, s.hotplugConfig); err != nil {
		if rollbackErr := csiControllerUnpublish(ctx, socketPath, s.host, info.VolumeHandle); rollbackErr != nil {
			log.DefaultLogger().Warningf("rollback ControllerUnpublish after NodeStage failure for %s: %v", resolvedHandle, rollbackErr)
		}
		return fmt.Errorf("CSI node stage for persistentRegional volume %s: %w", resolvedHandle, err)
	}

	if err := csiNodePublish(ctx, s.kubeletPodsDir, info, volumeName, csiTargetPath, s.hotplugConfig, encrypted); err != nil {
		if rollbackErr := csiNodeUnstage(ctx, socketPath, s.kubeletPodsDir, info.VolumeHandle, volumeName); rollbackErr != nil {
			log.DefaultLogger().Warningf("rollback NodeUnstage after NodePublish failure for %s: %v", resolvedHandle, rollbackErr)
		}
		if rollbackErr := csiControllerUnpublish(ctx, socketPath, s.host, info.VolumeHandle); rollbackErr != nil {
			log.DefaultLogger().Warningf("rollback ControllerUnpublish after NodePublish failure for %s: %v", resolvedHandle, rollbackErr)
		}
		return fmt.Errorf("CSI node publish for persistentRegional volume %s: %w", resolvedHandle, err)
	}

	if err := publishToLauncher(vmi, s.host, s.kubeletPodsDir, volumeName, hostCSITargetPath); err != nil {
		if rollbackErr := csiNodeUnpublish(ctx, socketPath, info.VolumeHandle, csiTargetPath); rollbackErr != nil {
			log.DefaultLogger().Warningf("rollback NodeUnpublish after publishToLauncher failure for %s: %v", resolvedHandle, rollbackErr)
		}
		if rollbackErr := csiNodeUnstage(ctx, socketPath, s.kubeletPodsDir, info.VolumeHandle, volumeName); rollbackErr != nil {
			log.DefaultLogger().Warningf("rollback NodeUnstage after publishToLauncher failure for %s: %v", resolvedHandle, rollbackErr)
		}
		if rollbackErr := csiControllerUnpublish(ctx, socketPath, s.host, info.VolumeHandle); rollbackErr != nil {
			log.DefaultLogger().Warningf("rollback ControllerUnpublish after publishToLauncher failure for %s: %v", resolvedHandle, rollbackErr)
		}
		return fmt.Errorf("publish to launcher for persistentRegional volume %s: %w", resolvedHandle, err)
	}

	return nil
}

func (s *Server) attachEphemeral(ctx context.Context, vmi *v1.VirtualMachineInstance, ns, vmiName, volumeName string, launcherUID types.UID, eph *v1.EphemeralLocalCustomVolumeSource) error {
	socketPath := s.hotplugConfig.Ephemeral.CSISocketPath
	stagingPath := s.hotplugConfig.Ephemeral.StagingPath

	qty := resource.MustParse(eph.Size)
	resp, err := s.createVolume(ctx, vmi, qty.Value(), socketPath, volumeName)
	if err != nil {
		return fmt.Errorf("create volume: %w", err)
	}

	csiVolumeID := resp.Volume.VolumeId
	if err := persistCSIVolumeID(stagingPath, string(vmi.UID), volumeName, csiVolumeID); err != nil {
		return fmt.Errorf("persist CSI volume ID: %w", err)
	}

	if err := s.ephemeralNodeStage(ctx, csiVolumeID, vmi, volumeName, resp.Volume.VolumeContext, stagingPath, socketPath); err != nil {
		return fmt.Errorf("node stage: %w", err)
	}

	csiTargetPath := csiPublishTargetPath(s.kubeletPodsDir, string(launcherUID), volumeName)
	hostCSITargetPath := filepath.Join(util.HostRootMount, csiTargetPath)
	if err := os.MkdirAll(hostCSITargetPath, 0750); err != nil {
		return fmt.Errorf("create CSI publish target dir: %w", err)
	}

	if err := s.ephemeralNodePublish(ctx, csiVolumeID, vmi, volumeName, socketPath, stagingPath, csiTargetPath, resp.Volume.VolumeContext); err != nil {
		return fmt.Errorf("node publish: %w", err)
	}

	return publishToLauncher(vmi, s.host, s.kubeletPodsDir, volumeName, hostCSITargetPath)
}

func (s *Server) exposeDevice(ctx context.Context, vmi *v1.VirtualMachineInstance, volumeName, targetPath string) error {
	var dev uint64
	dev, err := getDevice(targetPath)
	if err != nil {
		return err
	}

	deviceRule := &devices.Rule{
		Type:        devices.BlockDevice,
		Major:       int64(unix.Major(dev)),
		Minor:       int64(unix.Minor(dev)),
		Permissions: "rwm",
		Allow:       true,
	}

	var cgroupManager cgroup.Manager
	cgroupManager, err = getCgroupManager(vmi, s.host)
	if err != nil {
		return err
	}

	err = cgroupManager.Set(&configs.Resources{
		Devices: []*devices.Rule{deviceRule},
	})
	if err != nil {
		return err
	}
	var devicePath *safepath.Path
	devicePath, err = safepath.NewPathNoFollow(targetPath)
	if err != nil {
		return err
	}

	err = diskutils.DefaultOwnershipManager.SetFileOwnership(devicePath)
	if err != nil {
		return err
	}

	log.DefaultLogger().Infof("Device created for volume %s", volumeName)

	return nil
}

var getCgroupManager = func(vmi *v1.VirtualMachineInstance, host string) (cgroup.Manager, error) {
	return cgroup.NewManagerFromVM(vmi, host, "", true)
}

func getDevice(targetPath string) (dev uint64, err error) {
	info, err := os.Stat(targetPath)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeDevice == 0 {
		return 0, fmt.Errorf("%s is not a block device", targetPath)
	}
	fileInfo := info.Sys().(*syscall.Stat_t)

	return uint64(fileInfo.Rdev), nil
}

func (s *Server) createVolume(ctx context.Context, vmi *v1.VirtualMachineInstance, size int64, socketPath, volumeName string) (*csi.CreateVolumeResponse, error) {
	csiReq := &csi.CreateVolumeRequest{
		Name: getLocalVolumeName(string(vmi.UID), volumeName),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: size,
		},
		Parameters: map[string]string{
			"kubevirt.io/hotplug-volume": "true",
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
			},
		},
	}
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to CSI driver: %w", err)
	}
	defer conn.Close()

	client := csi.NewControllerClient(conn)
	resp, err := client.CreateVolume(ctx, csiReq)
	if err != nil {
		return nil, fmt.Errorf("CSI CreateVolume: %w", err)
	}

	return resp, nil
}

func getLocalVolumeName(uid, volumeName string) string {
	return "local-hotplug-" + uid + "-" + volumeName
}

func ephemeralStagingPath(baseStagingPath, vmiUID, volumeName string) string {
	result := sha256.Sum256([]byte(getLocalVolumeName(vmiUID, volumeName)))
	volSha := fmt.Sprintf("%x", result)
	return filepath.Join(baseStagingPath, volSha, "globalmount")
}

// ephemeralVolumeIDDir returns the base directory (without "globalmount") for
// a given ephemeral volume, used to store metadata like the CSI volume ID.
func ephemeralVolumeIDDir(baseStagingPath, vmiUID, volumeName string) string {
	result := sha256.Sum256([]byte(getLocalVolumeName(vmiUID, volumeName)))
	volSha := fmt.Sprintf("%x", result)
	return filepath.Join(baseStagingPath, volSha)
}

func persistCSIVolumeID(baseStagingPath, vmiUID, volumeName, csiVolumeID string) error {
	dir := ephemeralVolumeIDDir(baseStagingPath, vmiUID, volumeName)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create volume ID dir: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "csi-volume-id"), []byte(csiVolumeID), 0600)
}

func readCSIVolumeID(baseStagingPath, vmiUID, volumeName string) (string, error) {
	p := filepath.Join(ephemeralVolumeIDDir(baseStagingPath, vmiUID, volumeName), "csi-volume-id")
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read persisted CSI volume ID from %s: %w", p, err)
	}
	return string(data), nil
}

func ephemeralNodeUnstage(ctx context.Context, socketPath, csiVolumeID, stagingPath string) error {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver: %w", err)
	}
	defer conn.Close()

	client := csi.NewNodeClient(conn)
	_, err = client.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          csiVolumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		return fmt.Errorf("CSI NodeUnstageVolume for %s: %w", csiVolumeID, err)
	}

	os.RemoveAll(filepath.Dir(stagingPath))
	return nil
}

func (s *Server) ephemeralNodeStage(ctx context.Context, csiVolumeID string, vmi *v1.VirtualMachineInstance, volumeName string, volumeContext map[string]string, baseStagingPath string, socketPath string) error {
	stagingPathFinal := ephemeralStagingPath(baseStagingPath, string(vmi.UID), volumeName)
	if err := os.MkdirAll(filepath.Dir(stagingPathFinal), 0755); err != nil {
		return err
	}

	csiReq := csi.NodeStageVolumeRequest{
		VolumeId: csiVolumeID,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		StagingTargetPath: stagingPathFinal,
		VolumeContext:     volumeContext,
	}

	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver: %w", err)
	}
	defer conn.Close()

	client := csi.NewNodeClient(conn)
	_, err = client.NodeStageVolume(ctx, &csiReq)
	if err != nil {
		return fmt.Errorf("CSI NodeStageVolume: %w", err)
	}

	return nil
}

func (s *Server) ephemeralNodePublish(ctx context.Context, csiVolumeID string, vmi *v1.VirtualMachineInstance, volumeName string, socketPath string, baseStagingPath string, targetPath string, volumeContext map[string]string) error {
	stagingPathFinal := ephemeralStagingPath(baseStagingPath, string(vmi.UID), volumeName)

	csiReq := csi.NodePublishVolumeRequest{
		VolumeId:   csiVolumeID,
		TargetPath: targetPath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		StagingTargetPath: stagingPathFinal,
		VolumeContext:     volumeContext,
	}

	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver: %w", err)
	}
	defer conn.Close()

	client := csi.NewNodeClient(conn)
	_, err = client.NodePublishVolume(ctx, &csiReq)
	if err != nil {
		return fmt.Errorf("CSI NodePublishVolume: %w", err)
	}

	return nil
}

func (s *Server) removeVolume(ctx context.Context, vmi *v1.VirtualMachineInstance, volumeName string, socketPath string) error {
	return s.removeVolumeByID(ctx, getLocalVolumeName(string(vmi.UID), volumeName), socketPath)
}

func (s *Server) removeVolumeByID(ctx context.Context, csiVolumeID string, socketPath string) error {
	csiReq := csi.DeleteVolumeRequest{
		VolumeId: csiVolumeID,
	}

	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver: %w", err)
	}
	defer conn.Close()

	client := csi.NewControllerClient(conn)
	_, err = client.DeleteVolume(ctx, &csiReq)
	if err != nil {
		return fmt.Errorf("CSI DeleteVolume: %w", err)
	}

	return nil
}

