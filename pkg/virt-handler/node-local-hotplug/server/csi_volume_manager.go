package nodelocalhotplug

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// csiVolumeInfo holds everything needed to call CSI RPCs for a PV.
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

// publishCSIVolume performs the full CSI lifecycle for a PVC-backed CSI volume:
//  1. If driver requires attach: create VolumeAttachment CR, wait for external-attacher
//  2. NodeStage via the driver's node plugin socket
//  3. NodePublish via the driver's node plugin socket
func publishCSIVolume(ctx context.Context, virtCli kubecli.KubevirtClient, kubeletPodsDir, nodeName string, pv *k8sv1.PersistentVolume, targetPath string) error {
	info, err := extractCSIVolumeInfo(ctx, virtCli, pv)
	if err != nil {
		return err
	}

	if driverRequiresAttach(ctx, virtCli, info.Driver) {
		if _, err := ensureVolumeAttachment(ctx, virtCli, nodeName, pv, info); err != nil {
			return fmt.Errorf("ControllerPublish (VolumeAttachment) for PV %s: %w", pv.Name, err)
		}
	}

	if err := csiNodeStage(ctx, kubeletPodsDir, info, pv.Name); err != nil {
		return err
	}

	if err := csiNodePublish(ctx, kubeletPodsDir, info, pv.Name, targetPath); err != nil {
		return err
	}

	log.DefaultLogger().V(3).Infof("CSI publish complete for PV %s (driver=%s) at %s", pv.Name, info.Driver, targetPath)
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
		log.DefaultLogger().V(3).Infof("CSIDriver %s has attachRequired=false, skipping VolumeAttachment", driverName)
		return false
	}
	return true
}

func csiStagingTargetPath(kubeletPodsDir, pvName string) string {
	kubeletRoot := kubeletRootFromPodsDir(kubeletPodsDir)
	return filepath.Join(kubeletRoot, "plugins", "kubevirt.io", "node-local-hotplug", "staging", pvName)
}

func csiNodePluginSocketPath(kubeletPodsDir, driverName string) string {
	kubeletRoot := kubeletRootFromPodsDir(kubeletPodsDir)
	return filepath.Join(util.HostRootMount, kubeletRoot, "plugins", driverName, "csi.sock")
}

func newCSINodeClient(socketPath string) (csi.NodeClient, *grpc.ClientConn, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial CSI node plugin at %s: %w", socketPath, err)
	}
	return csi.NewNodeClient(conn), conn, nil
}

func csiNodeStage(ctx context.Context, kubeletPodsDir string, info *csiVolumeInfo, pvName string) error {
	socketPath := csiNodePluginSocketPath(kubeletPodsDir, info.Driver)
	if _, err := os.Stat(socketPath); err != nil {
		return fmt.Errorf("CSI node plugin socket not found at %s for driver %s: %w", socketPath, info.Driver, err)
	}

	stagingPath := csiStagingTargetPath(kubeletPodsDir, pvName)
	hostStagingPath := filepath.Join(util.HostRootMount, stagingPath)
	if err := os.MkdirAll(hostStagingPath, 0750); err != nil {
		return fmt.Errorf("create staging dir %s: %w", hostStagingPath, err)
	}

	nodeClient, conn, err := newCSINodeClient(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI node plugin %s: %w", info.Driver, err)
	}
	defer conn.Close()

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          info.VolumeHandle,
		StagingTargetPath: stagingPath,
		VolumeCapability:  buildVolumeCapability(info),
		VolumeContext:     info.VolumeAttributes,
		Secrets:           info.NodeStageSecrets,
	}

	_, err = nodeClient.NodeStageVolume(ctx, req)
	if err != nil {
		return fmt.Errorf("CSI NodeStageVolume for %s: %w", info.VolumeHandle, err)
	}

	log.DefaultLogger().V(3).Infof("CSI NodeStageVolume succeeded for %s (driver=%s)", info.VolumeHandle, info.Driver)
	return nil
}

func csiNodePublish(ctx context.Context, kubeletPodsDir string, info *csiVolumeInfo, pvName, targetPath string) error {
	socketPath := csiNodePluginSocketPath(kubeletPodsDir, info.Driver)

	nodeClient, conn, err := newCSINodeClient(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI node plugin %s: %w", info.Driver, err)
	}
	defer conn.Close()

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

	_, err = nodeClient.NodePublishVolume(ctx, req)
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

func csiNodeUnpublish(ctx context.Context, kubeletPodsDir string, info *csiVolumeInfo, targetPath string) error {
	socketPath := csiNodePluginSocketPath(kubeletPodsDir, info.Driver)

	nodeClient, conn, err := newCSINodeClient(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI node plugin %s: %w", info.Driver, err)
	}
	defer conn.Close()

	req := &csi.NodeUnpublishVolumeRequest{
		VolumeId:   info.VolumeHandle,
		TargetPath: targetPath,
	}

	_, err = nodeClient.NodeUnpublishVolume(ctx, req)
	if err != nil {
		return fmt.Errorf("CSI NodeUnpublishVolume for %s at %s: %w", info.VolumeHandle, targetPath, err)
	}

	log.DefaultLogger().V(3).Infof("CSI NodeUnpublishVolume succeeded for %s at %s", info.VolumeHandle, targetPath)
	return nil
}

func csiNodeUnstage(ctx context.Context, kubeletPodsDir string, info *csiVolumeInfo, pvName string) error {
	socketPath := csiNodePluginSocketPath(kubeletPodsDir, info.Driver)

	nodeClient, conn, err := newCSINodeClient(socketPath)
	if err != nil {
		return fmt.Errorf("connect to CSI node plugin %s: %w", info.Driver, err)
	}
	defer conn.Close()

	stagingPath := csiStagingTargetPath(kubeletPodsDir, pvName)
	req := &csi.NodeUnstageVolumeRequest{
		VolumeId:          info.VolumeHandle,
		StagingTargetPath: stagingPath,
	}

	_, err = nodeClient.NodeUnstageVolume(ctx, req)
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
