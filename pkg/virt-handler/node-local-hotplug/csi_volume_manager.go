package nodelocalhotplug

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	k8sv1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	"kubevirt.io/kubevirt/pkg/util"
)

const (
	volumeAttachmentPrefix  = "kubevirt-hotplug-"
	volumeAttachmentTimeout = 2 * time.Minute
	volumeAttachmentPoll    = 2 * time.Second
)

// csiVolumeInfo holds everything needed to call CSI RPCs for a volume.
// Built either from an existing PV or directly from a CreateVolume response.
type csiVolumeInfo struct {
	Driver           string
	VolumeHandle     string
	VolumeAttributes map[string]string
	ReadOnly         bool
	FSType           string
	VolumeMode       k8sv1.PersistentVolumeMode

	ControllerPublishSecrets map[string]string
	NodeStageSecrets         map[string]string
	NodePublishSecrets       map[string]string
}

func volumeAttachmentName(pvName, nodeName string) string {
	return fmt.Sprintf("%s-%s", pvName, nodeName)
}

// ensureVolumeAttachment creates a VolumeAttachment CR and waits for the
// external-attacher to mark it as attached. Returns the attachment metadata.
func ensureVolumeAttachment(ctx context.Context, virtCli kubecli.KubevirtClient, nodeName string, pv *k8sv1.PersistentVolume, info *csiVolumeInfo) (map[string]string, error) {
	vaName := volumeAttachmentName(pv.Name, nodeName)

	existing, err := virtCli.StorageV1().VolumeAttachments().Get(ctx, vaName, metav1.GetOptions{})
	if err == nil && existing.Status.Attached {
		log.DefaultLogger().V(3).Infof("VolumeAttachment %s already attached", vaName)
		return existing.Status.AttachmentMetadata, nil
	}

	if err != nil {
		pvName := pv.Name
		va := &storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{
				Name: vaName,
				Labels: map[string]string{
					"kubevirt.io/created-by": "node-local-hotplug",
				},
			},
			Spec: storagev1.VolumeAttachmentSpec{
				Attacher: info.Driver,
				Source: storagev1.VolumeAttachmentSource{
					PersistentVolumeName: &pvName,
				},
				NodeName: nodeName,
			},
		}

		existing, err = virtCli.StorageV1().VolumeAttachments().Create(ctx, va, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("create VolumeAttachment %s: %w", vaName, err)
		}
		log.DefaultLogger().V(3).Infof("Created VolumeAttachment %s for PV %s on node %s", vaName, pv.Name, nodeName)
	}

	metadata, werr := waitForVolumeAttachment(ctx, virtCli, vaName)
	if werr != nil {
		return nil, fmt.Errorf("wait for VolumeAttachment %s: %w", vaName, werr)
	}
	return metadata, nil
}

func waitForVolumeAttachment(ctx context.Context, virtCli kubecli.KubevirtClient, vaName string) (map[string]string, error) {
	var metadata map[string]string
	err := wait.PollUntilContextTimeout(ctx, volumeAttachmentPoll, volumeAttachmentTimeout, true, func(ctx context.Context) (bool, error) {
		va, gerr := virtCli.StorageV1().VolumeAttachments().Get(ctx, vaName, metav1.GetOptions{})
		if gerr != nil {
			return false, nil
		}
		if va.Status.AttachError != nil {
			return false, fmt.Errorf("attach error: %s", va.Status.AttachError.Message)
		}
		if va.Status.Attached {
			metadata = va.Status.AttachmentMetadata
			return true, nil
		}
		return false, nil
	})
	return metadata, err
}

// csiCreateVolume calls CreateVolume on the CSI driver socket.
// Returns the CSI Volume (containing VolumeId, capacity, etc.).
func csiCreateVolume(ctx context.Context, socketPath string, pvc *k8sv1.PersistentVolumeClaim, sc *storagev1.StorageClass, nodeName string) (*csi.Volume, error) {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to CSI driver %s: %w", sc.Provisioner, err)
	}
	defer conn.Close()

	storageReq := pvc.Spec.Resources.Requests[k8sv1.ResourceStorage]
	capacityBytes := storageReq.Value()

	accessMode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	if len(pvc.Spec.AccessModes) > 0 {
		switch pvc.Spec.AccessModes[0] {
		case k8sv1.ReadWriteMany:
			accessMode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		case k8sv1.ReadOnlyMany:
			accessMode = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
		case k8sv1.ReadWriteOncePod:
			accessMode = csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER
		}
	}

	volCap := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: accessMode},
	}
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == k8sv1.PersistentVolumeBlock {
		volCap.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
	} else {
		fsType := "ext4"
		if sc.Parameters != nil {
			if ft, ok := sc.Parameters["fsType"]; ok {
				fsType = ft
			}
			if ft, ok := sc.Parameters["csi.storage.k8s.io/fstype"]; ok {
				fsType = ft
			}
		}
		volCap.AccessType = &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: fsType}}
	}

	req := &csi.CreateVolumeRequest{
		Name:               pvc.Name,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: capacityBytes},
		VolumeCapabilities: []*csi.VolumeCapability{volCap},
		Parameters:         sc.Parameters,
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{{Segments: map[string]string{"topology.kubernetes.io/hostname": nodeName}}},
		},
	}

	client := csi.NewControllerClient(conn)
	resp, err := client.CreateVolume(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("CSI CreateVolume for PVC %s/%s: %w", pvc.Namespace, pvc.Name, err)
	}

	log.DefaultLogger().V(3).Infof("CSI CreateVolume succeeded: volumeId=%s capacity=%d",
		resp.GetVolume().GetVolumeId(), resp.GetVolume().GetCapacityBytes())
	return resp.GetVolume(), nil
}

// csiDeleteVolume calls DeleteVolume on the CSI driver socket.
func csiDeleteVolume(ctx context.Context, socketPath, volumeID string) error {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := csi.NewControllerClient(conn)
	_, err = client.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID})
	if err != nil {
		return fmt.Errorf("CSI DeleteVolume %s: %w", volumeID, err)
	}
	log.DefaultLogger().V(3).Infof("CSI DeleteVolume succeeded for %s", volumeID)
	return nil
}

// provisionCSIVolume calls CreateVolume on the CSI driver and builds a
// csiVolumeInfo directly from the response — no PV is created.
func provisionCSIVolume(ctx context.Context, socketPath, nodeName string, pvc *k8sv1.PersistentVolumeClaim, sc *storagev1.StorageClass) (*csiVolumeInfo, error) {
	vol, err := csiCreateVolume(ctx, socketPath, pvc, sc, nodeName)
	if err != nil {
		return nil, err
	}

	volumeMode := k8sv1.PersistentVolumeFilesystem
	if pvc.Spec.VolumeMode != nil {
		volumeMode = *pvc.Spec.VolumeMode
	}

	fsType := fsTypeFromStorageClass(sc)

	info := &csiVolumeInfo{
		Driver:           sc.Provisioner,
		VolumeHandle:     vol.GetVolumeId(),
		VolumeAttributes: vol.GetVolumeContext(),
		FSType:           fsType,
		VolumeMode:       volumeMode,
	}

	log.DefaultLogger().V(3).Infof("Provisioned CSI volume %s (driver=%s) for PVC %s/%s — no PV created",
		info.VolumeHandle, info.Driver, pvc.Namespace, pvc.Name)
	return info, nil
}

func fsTypeFromStorageClass(sc *storagev1.StorageClass) string {
	if sc.Parameters != nil {
		if ft, ok := sc.Parameters["csi.storage.k8s.io/fstype"]; ok {
			return ft
		}
		if ft, ok := sc.Parameters["fsType"]; ok {
			return ft
		}
	}
	return "ext4"
}

// publishCSIVolume performs the CSI attach+mount lifecycle using the provided
// csiVolumeInfo (which may have been built from an existing PV or directly
// from a CreateVolume response).
//
// The CSI driver socket is derived from kubeletPodsDir + info.Driver
// (convention: /var/lib/kubelet/plugins/<driver>/csi.sock). If the socket
// is not found, returns (false, nil) and the caller must treat that as a hard error
// for node-local hotplug (the VMI is already marked NodeLocal).
func publishCSIVolume(ctx context.Context, virtCli kubecli.KubevirtClient, kubeletPodsDir, nodeName string, info *csiVolumeInfo, targetPath string) (bool, error) {
	socketPath := csiNodePluginSocketPath(kubeletPodsDir, info.Driver)
	if _, err := os.Stat(socketPath); err != nil {
		log.DefaultLogger().V(3).Infof(
			"CSI socket not found at %s for driver %s; deferring NodeStage/NodePublish",
			socketPath, info.Driver)
		return false, nil
	}

	if driverRequiresAttach(ctx, virtCli, info.Driver) && driverSupportsControllerPublish(ctx, socketPath) {
		if _, err := csiControllerPublish(ctx, socketPath, nodeName, info); err != nil {
			return false, fmt.Errorf("ControllerPublish for volume %s: %w", info.VolumeHandle, err)
		}
	}

	if err := csiNodeStage(ctx, socketPath, kubeletPodsDir, info, info.VolumeHandle); err != nil {
		return false, err
	}

	if err := csiNodePublish(ctx, socketPath, kubeletPodsDir, info, info.VolumeHandle, targetPath); err != nil {
		return false, err
	}

	log.DefaultLogger().V(3).Infof("CSI publish complete for volume %s (driver=%s) at %s", info.VolumeHandle, info.Driver, targetPath)
	return true, nil
}

// unpublishCSIVolume reverses publishCSIVolume: NodeUnpublish, NodeUnstage, then
// ControllerUnpublish when the driver requires attach. Best-effort removal of
// the per-volume publish directory follows.
func unpublishCSIVolume(ctx context.Context, virtCli kubecli.KubevirtClient, kubeletPodsDir, nodeName string, info *csiVolumeInfo, targetPath string) error {
	socketPath := csiNodePluginSocketPath(kubeletPodsDir, info.Driver)
	if _, err := os.Stat(socketPath); err != nil {
		return fmt.Errorf("stat CSI socket %s: %w", socketPath, err)
	}

	if err := csiNodeUnpublish(ctx, socketPath, info, targetPath); err != nil {
		return err
	}

	if err := csiNodeUnstage(ctx, socketPath, kubeletPodsDir, info, info.VolumeHandle); err != nil {
		return err
	}

	if driverRequiresAttach(ctx, virtCli, info.Driver) && driverSupportsControllerPublish(ctx, socketPath) {
		if err := csiControllerUnpublish(ctx, socketPath, nodeName, info); err != nil {
			return err
		}
	}

	hostRoot := strings.TrimSuffix(util.HostRootMount, "/")
	hostPublish := filepath.Join(hostRoot, strings.TrimPrefix(filepath.Clean(targetPath), "/"))
	if err := os.RemoveAll(hostPublish); err != nil {
		log.DefaultLogger().V(3).Infof("remove CSI publish dir %s: %v", hostPublish, err)
	}
	return nil
}

func extractCSIVolumeInfo(ctx context.Context, virtCli kubecli.KubevirtClient, pv *k8sv1.PersistentVolume) (*csiVolumeInfo, error) {
	if pv.Spec.CSI == nil {
		return nil, fmt.Errorf("PV %s is not a CSI volume", pv.Name)
	}

	csiSpec := pv.Spec.CSI
	info := &csiVolumeInfo{
		Driver:           csiSpec.Driver,
		VolumeHandle:     csiSpec.VolumeHandle,
		VolumeAttributes: csiSpec.VolumeAttributes,
		ReadOnly:         csiSpec.ReadOnly,
		FSType:           csiSpec.FSType,
		VolumeMode:       k8sv1.PersistentVolumeFilesystem,
	}
	if pv.Spec.VolumeMode != nil {
		info.VolumeMode = *pv.Spec.VolumeMode
	}

	var err error
	if csiSpec.ControllerPublishSecretRef != nil {
		info.ControllerPublishSecrets, err = resolveSecretData(ctx, virtCli, csiSpec.ControllerPublishSecretRef)
		if err != nil {
			return nil, fmt.Errorf("resolve ControllerPublishSecretRef for PV %s: %w", pv.Name, err)
		}
	}
	if csiSpec.NodeStageSecretRef != nil {
		info.NodeStageSecrets, err = resolveSecretData(ctx, virtCli, csiSpec.NodeStageSecretRef)
		if err != nil {
			return nil, fmt.Errorf("resolve NodeStageSecretRef for PV %s: %w", pv.Name, err)
		}
	}
	if csiSpec.NodePublishSecretRef != nil {
		info.NodePublishSecrets, err = resolveSecretData(ctx, virtCli, csiSpec.NodePublishSecretRef)
		if err != nil {
			return nil, fmt.Errorf("resolve NodePublishSecretRef for PV %s: %w", pv.Name, err)
		}
	}

	return info, nil
}

func resolveSecretData(ctx context.Context, virtCli kubecli.KubevirtClient, ref *k8sv1.SecretReference) (map[string]string, error) {
	if ref.Name == "" {
		return nil, nil
	}
	secret, err := virtCli.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		result[k] = string(v)
	}
	return result, nil
}

func driverRequiresAttach(ctx context.Context, virtCli kubecli.KubevirtClient, driverName string) bool {
	csiDriver, err := virtCli.StorageV1().CSIDrivers().Get(ctx, driverName, metav1.GetOptions{})
	if err != nil {
		log.DefaultLogger().V(3).Infof("CSIDriver %s not found, assuming attachRequired=true: %v", driverName, err)
		return true
	}
	if csiDriver.Spec.AttachRequired != nil && !*csiDriver.Spec.AttachRequired {
		log.DefaultLogger().V(3).Infof("CSIDriver %s has attachRequired=false, skipping ControllerPublish", driverName)
		return false
	}
	return true
}

// driverSupportsControllerPublish probes the CSI driver's actual controller
// capabilities via gRPC. Some drivers declare attachRequired in their CSIDriver
// object but don't implement ControllerPublishVolume (e.g. csi-hostpath).
func driverSupportsControllerPublish(ctx context.Context, socketPath string) bool {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return true
	}
	defer conn.Close()

	client := csi.NewControllerClient(conn)
	resp, err := client.ControllerGetCapabilities(ctx, &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		log.DefaultLogger().V(3).Infof("ControllerGetCapabilities failed, assuming publish supported: %v", err)
		return true
	}
	for _, cap := range resp.GetCapabilities() {
		if rpc := cap.GetRpc(); rpc != nil && rpc.GetType() == csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME {
			return true
		}
	}
	log.DefaultLogger().V(3).Infof("CSI driver does not advertise PUBLISH_UNPUBLISH_VOLUME capability")
	return false
}

func csiStagingTargetPath(kubeletPodsDir, pvName string) string {
	kubeletRoot := kubeletRootFromPodsDir(kubeletPodsDir)
	return filepath.Join(kubeletRoot, "plugins", "kubevirt.io", "node-local-hotplug", "staging", pvName)
}

// csiNodePluginSocketPath finds the CSI node plugin socket for a driver.
//
// CSI drivers don't always use their driver name as the plugin directory name
// (e.g. "hostpath.csi.k8s.io" may live at plugins/csi-hostpath/csi.sock).
//
// Strategy:
//  1. Try conventional path: plugins/<driverName>/csi.sock
//  2. Scan plugins/ subdirectories for csi.sock and call CSI Identity
//     GetPluginInfo to match the driver name
func csiNodePluginSocketPath(kubeletPodsDir, driverName string) string {
	kubeletRoot := kubeletRootFromPodsDir(kubeletPodsDir)
	pluginsDir := filepath.Join(util.HostRootMount, kubeletRoot, "plugins")

	conventional := filepath.Join(pluginsDir, driverName, "csi.sock")
	if _, err := os.Stat(conventional); err == nil {
		return conventional
	}

	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return conventional
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(pluginsDir, entry.Name(), "csi.sock")
		if _, serr := os.Stat(candidate); serr != nil {
			continue
		}
		name, ierr := csiGetPluginName(candidate)
		if ierr != nil {
			log.DefaultLogger().V(5).Infof("Skipping %s: GetPluginInfo failed: %v", candidate, ierr)
			continue
		}
		if name == driverName {
			log.DefaultLogger().V(3).Infof("CSI socket for driver %s found at %s", driverName, candidate)
			return candidate
		}
	}

	return conventional
}

// csiGetPluginName calls CSI Identity.GetPluginInfo on a socket and returns
// the driver name reported by the plugin.
func csiGetPluginName(socketPath string) (string, error) {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	client := csi.NewIdentityClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
	if err != nil {
		return "", err
	}
	return resp.GetName(), nil
}

// csiDriverFromPVC extracts the CSI driver name from a PVC. It checks the
// standard provisioner annotation first (no API calls needed), then falls
// back to looking up the StorageClass provisioner.
func csiDriverFromPVC(ctx context.Context, virtCli kubecli.KubevirtClient, pvc *k8sv1.PersistentVolumeClaim) string {
	for _, key := range []string{
		"volume.kubernetes.io/storage-provisioner",
		"volume.beta.kubernetes.io/storage-provisioner",
	} {
		if driver, ok := pvc.Annotations[key]; ok && driver != "" {
			return driver
		}
	}
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
		sc, err := virtCli.StorageV1().StorageClasses().Get(ctx, *pvc.Spec.StorageClassName, metav1.GetOptions{})
		if err == nil {
			return sc.Provisioner
		}
	}
	return ""
}

// csiSocketPathFromPVC derives the CSI node plugin socket path for a PVC by
// extracting the driver name from the PVC annotations or StorageClass and
// building the conventional kubelet plugins path.
func csiSocketPathFromPVC(ctx context.Context, virtCli kubecli.KubevirtClient, kubeletPodsDir string, pvc *k8sv1.PersistentVolumeClaim) string {
	driver := csiDriverFromPVC(ctx, virtCli, pvc)
	if driver == "" {
		return ""
	}
	return csiNodePluginSocketPath(kubeletPodsDir, driver)
}

// newCSIConn dials the CSI driver Unix socket shared by both Controller and
// Node gRPC services.
func newCSIConn(socketPath string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial CSI driver at %s: %w", socketPath, err)
	}
	return conn, nil
}

// csiControllerPublish calls ControllerPublishVolume directly on the CSI
// driver's controller socket, bypassing the VolumeAttachment CR and
// external-attacher sidecar.
func csiControllerPublish(ctx context.Context, socketPath, nodeName string, info *csiVolumeInfo) (map[string]string, error) {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to CSI driver %s: %w", info.Driver, err)
	}
	defer conn.Close()

	client := csi.NewControllerClient(conn)
	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         info.VolumeHandle,
		NodeId:           nodeName,
		VolumeCapability: buildVolumeCapability(info),
		Readonly:         info.ReadOnly,
		Secrets:          info.ControllerPublishSecrets,
		VolumeContext:    info.VolumeAttributes,
	}

	resp, err := client.ControllerPublishVolume(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("CSI ControllerPublishVolume for %s on node %s: %w", info.VolumeHandle, nodeName, err)
	}

	log.DefaultLogger().V(3).Infof("CSI ControllerPublishVolume succeeded for %s on node %s (driver=%s)",
		info.VolumeHandle, nodeName, info.Driver)

	publishInfo := resp.GetPublishContext()
	return publishInfo, nil
}

// csiControllerUnpublish calls ControllerUnpublishVolume directly on the CSI
// driver socket.
func csiControllerUnpublish(ctx context.Context, socketPath, nodeName string, info *csiVolumeInfo) error {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver %s: %w", info.Driver, err)
	}
	defer conn.Close()

	client := csi.NewControllerClient(conn)
	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: info.VolumeHandle,
		NodeId:   nodeName,
		Secrets:  info.ControllerPublishSecrets,
	}

	_, err = client.ControllerUnpublishVolume(ctx, req)
	if err != nil {
		return fmt.Errorf("CSI ControllerUnpublishVolume for %s on node %s: %w", info.VolumeHandle, nodeName, err)
	}

	log.DefaultLogger().V(3).Infof("CSI ControllerUnpublishVolume succeeded for %s on node %s (driver=%s)",
		info.VolumeHandle, nodeName, info.Driver)
	return nil
}

func csiNodeStage(ctx context.Context, socketPath, kubeletPodsDir string, info *csiVolumeInfo, pvName string) error {
	stagingPath := csiStagingTargetPath(kubeletPodsDir, pvName)
	hostStagingPath := filepath.Join(util.HostRootMount, stagingPath)
	if err := os.MkdirAll(hostStagingPath, 0750); err != nil {
		return fmt.Errorf("create staging dir %s: %w", hostStagingPath, err)
	}

	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver %s: %w", info.Driver, err)
	}
	defer conn.Close()

	client := csi.NewNodeClient(conn)
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          info.VolumeHandle,
		StagingTargetPath: stagingPath,
		VolumeCapability:  buildVolumeCapability(info),
		VolumeContext:     info.VolumeAttributes,
		Secrets:           info.NodeStageSecrets,
	}

	_, err = client.NodeStageVolume(ctx, req)
	if err != nil {
		return fmt.Errorf("CSI NodeStageVolume for %s: %w", info.VolumeHandle, err)
	}

	log.DefaultLogger().V(3).Infof("CSI NodeStageVolume succeeded for %s (driver=%s)", info.VolumeHandle, info.Driver)
	return nil
}

func csiNodePublish(ctx context.Context, socketPath, kubeletPodsDir string, info *csiVolumeInfo, pvName, targetPath string) error {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver %s: %w", info.Driver, err)
	}
	defer conn.Close()

	client := csi.NewNodeClient(conn)
	stagingPath := csiStagingTargetPath(kubeletPodsDir, pvName)
	req := &csi.NodePublishVolumeRequest{
		VolumeId:          info.VolumeHandle,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  buildVolumeCapability(info),
		VolumeContext:     info.VolumeAttributes,
		Readonly:          info.ReadOnly,
		Secrets:           info.NodePublishSecrets,
	}

	_, err = client.NodePublishVolume(ctx, req)
	if err != nil {
		return fmt.Errorf("CSI NodePublishVolume for %s at %s: %w", info.VolumeHandle, targetPath, err)
	}

	log.DefaultLogger().V(3).Infof("CSI NodePublishVolume succeeded for %s at %s", info.VolumeHandle, targetPath)
	return nil
}

func buildVolumeCapability(info *csiVolumeInfo) *csi.VolumeCapability {
	accessMode := &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	}

	if info.VolumeMode == k8sv1.PersistentVolumeBlock {
		return &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: accessMode,
		}
	}

	mountVolume := &csi.VolumeCapability_MountVolume{}
	if info.FSType != "" {
		mountVolume.FsType = info.FSType
	}
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: mountVolume},
		AccessMode: accessMode,
	}
}

func csiNodeUnpublish(ctx context.Context, socketPath string, info *csiVolumeInfo, targetPath string) error {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver %s: %w", info.Driver, err)
	}
	defer conn.Close()

	client := csi.NewNodeClient(conn)
	req := &csi.NodeUnpublishVolumeRequest{
		VolumeId:   info.VolumeHandle,
		TargetPath: targetPath,
	}

	_, err = client.NodeUnpublishVolume(ctx, req)
	if err != nil {
		return fmt.Errorf("CSI NodeUnpublishVolume for %s at %s: %w", info.VolumeHandle, targetPath, err)
	}

	log.DefaultLogger().V(3).Infof("CSI NodeUnpublishVolume succeeded for %s at %s", info.VolumeHandle, targetPath)
	return nil
}

func csiNodeUnstage(ctx context.Context, socketPath, kubeletPodsDir string, info *csiVolumeInfo, pvName string) error {
	conn, err := newCSIConn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI driver %s: %w", info.Driver, err)
	}
	defer conn.Close()

	client := csi.NewNodeClient(conn)
	stagingPath := csiStagingTargetPath(kubeletPodsDir, pvName)
	req := &csi.NodeUnstageVolumeRequest{
		VolumeId:          info.VolumeHandle,
		StagingTargetPath: stagingPath,
	}

	_, err = client.NodeUnstageVolume(ctx, req)
	if err != nil {
		return fmt.Errorf("CSI NodeUnstageVolume for %s: %w", info.VolumeHandle, err)
	}

	hostStagingPath := filepath.Join(util.HostRootMount, stagingPath)
	os.RemoveAll(hostStagingPath)

	log.DefaultLogger().V(3).Infof("CSI NodeUnstageVolume succeeded for %s", info.VolumeHandle)
	return nil
}

func deleteVolumeAttachment(ctx context.Context, virtCli kubecli.KubevirtClient, pvName, nodeName string) error {
	vaName := volumeAttachmentName(pvName, nodeName)
	err := virtCli.StorageV1().VolumeAttachments().Delete(ctx, vaName, metav1.DeleteOptions{})
	if err != nil {
		log.DefaultLogger().V(3).Infof("delete VolumeAttachment %s (may not exist): %v", vaName, err)
	}
	return nil
}

