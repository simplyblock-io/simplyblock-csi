/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	mount "k8s.io/mount-utils"
	"k8s.io/utils/exec"

	csicommon "github.com/spdk/spdk-csi/pkg/csi-common"
	"github.com/spdk/spdk-csi/pkg/util"
)

type nodeServer struct {
	*csicommon.DefaultNodeServer
	mounter       mount.Interface
	volumeLocks   *util.VolumeLocks
	xpuConnClient *grpc.ClientConn
	xpuTargetType string
	kvmPciBridges int
	spdkNode      *util.NodeNVMf
}

func newNodeServer(d *csicommon.CSIDriver) (*nodeServer, error) {
	ns := &nodeServer{
		DefaultNodeServer: csicommon.NewDefaultNodeServer(d),
		mounter:           mount.New(""),
		volumeLocks:       util.NewVolumeLocks(),
	}

	// get xPU nodes' configs, see deploy/kubernetes/nodeserver-config-map.yaml
	// as spdkcsi-nodeservercm configMap volume is optional when deploying k8s, check nodeserver-config-map.yaml is missing or empty
	spdkcsiNodeServerConfigFile := "/etc/spdkcsi-nodeserver-config/nodeserver-config.json"
	spdkcsiNodeServerConfigFileEnv := "SPDKCSI_CONFIG_NODESERVER"
	configFile := util.FromEnv(spdkcsiNodeServerConfigFileEnv, spdkcsiNodeServerConfigFile)
	_, err := os.Stat(configFile)
	klog.Infof("check whether the configuration file (%s) which is supposed to contain xPU info exists", spdkcsiNodeServerConfigFile)
	if os.IsNotExist(err) {
		klog.Infof("configuration file specified in %s (%s by default) is missing or empty", spdkcsiNodeServerConfigFileEnv, spdkcsiNodeServerConfigFile)
		return ns, nil
	}
	//nolint:tagliatelle // not using json:snake case
	var config struct {
		XPUList []struct {
			Name       string `json:"name"`
			TargetType string `json:"targetType"`
			TargetAddr string `json:"targetAddr"`
		} `json:"xpuList"`
		KvmPciBridges int `json:"kvmPciBridges,omitempty"`
	}

	err = util.ParseJSONFile(configFile, &config)
	if err != nil {
		return nil, fmt.Errorf("error in the configuration file specified in %s (%s by default): %w", spdkcsiNodeServerConfigFileEnv, spdkcsiNodeServerConfigFile, err)
	}
	klog.Infof("obtained xPU info (%v) from configuration file (%s)", config.XPUList, spdkcsiNodeServerConfigFile)

	ns.kvmPciBridges = config.KvmPciBridges
	klog.Infof("obtained KvmPciBridges num (%v) from configuration file (%s)", config.KvmPciBridges, spdkcsiNodeServerConfigFile)

	// try to set up a connection to the first available xPU node in the list via grpc
	// once the connection is built, send pings every 10 seconds if there is no activity
	// FIXME (JingYan): when there are multiple xPU nodes, find a better way to choose one to connect with

	var xpuConnClient, conn *grpc.ClientConn
	var xpuTargetType string

	for i := range config.XPUList {
		if config.XPUList[i].TargetType != "" && config.XPUList[i].TargetAddr != "" {
			klog.Infof("TargetType: %v, TargetAddr: %v.", config.XPUList[i].TargetType, config.XPUList[i].TargetAddr)
			conn, err = grpc.Dial(
				config.XPUList[i].TargetAddr,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithBlock(),
				grpc.WithKeepaliveParams(keepalive.ClientParameters{
					Time:                10 * time.Second,
					Timeout:             1 * time.Second,
					PermitWithoutStream: true,
				}),
				grpc.FailOnNonTempDialError(true),
			)
			if err != nil {
				klog.Errorf("failed to connect to xPU node in: %s, %s", config.XPUList[i].TargetAddr, err)
			} else {
				klog.Infof("connected to xPU node %v with TargetType as %v", config.XPUList[i].TargetAddr, config.XPUList[i].TargetType)
				xpuConnClient = conn
				xpuTargetType = config.XPUList[i].TargetType
				break
			}
		} else {
			klog.Errorf("missing xPU TargetType or TargetAddr in xPUList index %d, skipping this xPU node", i)
		}
	}
	if xpuConnClient == nil && xpuTargetType == "" {
		klog.Infof("failed to connect to any xPU node in the xpuList or xpuList is empty, will continue without xPU node")
	}
	ns.xpuConnClient = xpuConnClient
	ns.xpuTargetType = xpuTargetType

	ns.spdkNode, err = NewsimplyBlockClient()
	if err != nil {
		klog.Error("failed to create simplyblock client", err)
		return nil, err
	}

	go util.MonitorConnection(ns.spdkNode)

	return ns, nil
}

func (ns *nodeServer) NodeGetVolumeStats(_ context.Context, _ *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *nodeServer) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath() // use this directory to persistently store VolumeContext
	stagingTargetPath := getStagingTargetPath(req)

	isStaged, err := ns.isStaged(stagingTargetPath)
	if err != nil {
		klog.Errorf("failed to check isStaged, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if isStaged {
		klog.Warning("volume already staged")
		return &csi.NodeStageVolumeResponse{}, nil
	}

	var initiator util.SpdkCsiInitiator
	vc := req.GetVolumeContext()

	vc["stagingParentPath"] = stagingParentPath
	initiator, err = util.NewSpdkCsiInitiator(vc, ns.spdkNode)
	if err != nil {
		klog.Errorf("failed to create spdk initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	devicePath, err := initiator.Connect() // idempotent
	if err != nil {
		klog.Errorf("failed to connect initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer func() {
		if err != nil {
			initiator.Disconnect() //nolint:errcheck // ignore error
		}
	}()
	if err = ns.stageVolume(devicePath, stagingTargetPath, req, vc); err != nil { // idempotent
		klog.Errorf("failed to stage volume, volumeID: %s devicePath:%s err: %v", volumeID, devicePath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	vc["devicePath"] = devicePath
	// stash VolumeContext to stagingParentPath (useful during Unstage as it has no
	// VolumeContext passed to the RPC as per the CSI spec)
	err = util.StashVolumeContext(req.GetVolumeContext(), stagingParentPath)
	if err != nil {
		klog.Errorf("failed to stash volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath()
	stagingTargetPath := getStagingTargetPath(req)

	err := ns.deleteMountPoint(stagingTargetPath) // idempotent
	if err != nil {
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Errorf(codes.Internal, "unstage volume %s failed: %s", volumeID, err)
	}

	volumeContext, err := util.LookupVolumeContext(stagingParentPath)
	if err != nil {
		klog.Errorf("failed to lookup volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	initiator, err := util.NewSpdkCsiInitiator(volumeContext, ns.spdkNode)
	if err != nil {
		klog.Errorf("failed to create spdk initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	err = initiator.Disconnect() // idempotent
	if err != nil {
		klog.Errorf("failed to disconnect initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := util.CleanUpVolumeContext(stagingParentPath); err != nil {
		klog.Errorf("failed to clean up volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	err := ns.publishVolume(getStagingTargetPath(req), req) // idempotent
	if err != nil {
		klog.Errorf("failed to publish volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	err := ns.deleteMountPoint(req.GetTargetPath()) // idempotent
	if err != nil {
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", req.GetTargetPath(), err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_VOLUME_CONDITION,
					},
				},
			},
		},
	}, nil
}

func (ns *nodeServer) NodeExpandVolume(_ context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	klog.Infof("NodeExpandVolume: called with args %+v", *req)

	volumeID := req.GetVolumeId()
	volumeMountPath := req.GetVolumePath()

	stagingParentPath := req.GetStagingTargetPath()
	volumeContext, err := util.LookupVolumeContext(stagingParentPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve volume context for volume %s: %v", volumeID, err)
	}

	devicePath, ok := volumeContext["devicePath"]
	if !ok || devicePath == "" {
		return nil, status.Errorf(codes.Internal, "could not find device path for volume %s", volumeID)
	}

	resizer := mount.NewResizeFs(exec.New())
	needsResize, err := resizer.NeedResize(devicePath, volumeMountPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if volume %s needs resizing: %v", volumeID, err)
	}

	if needsResize {
		resized, err := resizer.Resize(devicePath, volumeMountPath)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to resize volume %s: %v", volumeID, err)
		}
		if resized {
			klog.Infof("Successfully resized volume %s (device: %s, mount path: %s)", volumeID, devicePath, volumeMountPath)
		} else {
			klog.Warningf("Volume %s did not require resizing", volumeID)
		}
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

// must be idempotent
//
//nolint:cyclop // many cases in switch increases complexity
func (ns *nodeServer) stageVolume(devicePath, stagingPath string, req *csi.NodeStageVolumeRequest, volumeContext map[string]string) error {
	if req.GetVolumeCapability().GetBlock() != nil {
		klog.Infof("NodeStageVolume: called for volume %s. Skipping staging since it is a block device.", req.GetVolumeId())
		return nil
	}

	mounted, err := ns.createMountPoint(stagingPath)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	fsType := req.GetVolumeCapability().GetMount().GetFsType()
	// if fsType is not specified, use ext4 as default
	if fsType == "" {
		fsType = "ext4"
	}

	mntFlags := req.GetVolumeCapability().GetMount().GetMountFlags()
	formatOptions := []string{}

	if fsType == "xfs" {
		distrNdcs, errNdcs := strconv.Atoi(volumeContext["distr_ndcs"])
		if errNdcs != nil {
			return errNdcs
		}

		formatOptions = append(formatOptions, "-d", fmt.Sprintf("sunit=%d,swidth=%d", 8*distrNdcs, 8*distrNdcs), "-l", fmt.Sprintf("sunit=%d", 8*distrNdcs))

		// By default, xfs does not allow mounting of two volumes with the same filesystem uuid.
		// Force ignore this uuid to be able to mount volume + its clone / restored snapshot on the same node.
		mntFlags = append(mntFlags, "nouuid")
	}

	switch req.GetVolumeCapability().GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		mntFlags = append(mntFlags, "ro")
	case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return errors.New("unsupported MULTI_NODE_MULTI_WRITER AccessMode")
	case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER:
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
	case csi.VolumeCapability_AccessMode_UNKNOWN:
	}

	klog.Infof("mount %s to %s, fstype: %s, flags: %v", devicePath, stagingPath, fsType, mntFlags)
	klog.Infof("formatOptions %v", formatOptions)
	mounter := mount.SafeFormatAndMount{Interface: ns.mounter, Exec: exec.New()}
	err = mounter.FormatAndMountSensitiveWithFormatOptions(devicePath, stagingPath, fsType, mntFlags, nil, formatOptions)
	if err != nil {
		return err
	}
	return nil
}

// isStaged if stagingPath is a mount point, it means it is already staged, and vice versa
func (ns *nodeServer) isStaged(stagingPath string) (bool, error) {
	isMount, err := ns.mounter.IsMountPoint(stagingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		} else if mount.IsCorruptedMnt(err) {
			return true, nil
		}
		klog.Warningf("check is stage error: %v", err)
		return false, err
	}
	return isMount, nil
}

// must be idempotent
func (ns *nodeServer) publishVolume(stagingPath string, req *csi.NodePublishVolumeRequest) error {
	targetPath := req.GetTargetPath()

	fsType := req.GetVolumeCapability().GetMount().GetFsType()

	if req.GetVolumeCapability().GetBlock() != nil {
		stagingParentPath := req.GetStagingTargetPath()
		volumeContext, err := util.LookupVolumeContext(stagingParentPath)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to retrieve volume context for volume %s: %v", req.GetVolumeId(), err)
		}

		devicePath, ok := volumeContext["devicePath"]
		if !ok || devicePath == "" {
			return status.Errorf(codes.Internal, "could not find device path for volume %s", req.GetVolumeId())
		}
		stagingPath = devicePath

		fsType = ""
		if err = ns.MakeFile(targetPath); err != nil {
			if removeErr := os.Remove(targetPath); removeErr != nil {
				return status.Errorf(codes.Internal, "Could not remove mount target %q: %v", targetPath, removeErr)
			}
			return status.Errorf(codes.Internal, "Could not create file %q: %v", targetPath, err)
		}
	} else if req.GetVolumeCapability().GetMount() != nil {
		mounted, err := ns.createMountPoint(targetPath)
		if err != nil {
			return err
		}
		if mounted {
			return nil
		}
	}

	mntFlags := req.GetVolumeCapability().GetMount().GetMountFlags()
	mntFlags = append(mntFlags, "bind")
	klog.Infof("mount %s to %s, fstype: %s, flags: %v", stagingPath, targetPath, fsType, mntFlags)
	return ns.mounter.Mount(stagingPath, targetPath, fsType, mntFlags)
}

// create mount point if not exists, return whether already mounted
func (ns *nodeServer) createMountPoint(path string) (bool, error) {
	isMount, err := ns.mounter.IsMountPoint(path)
	if os.IsNotExist(err) {
		isMount = false
		err = os.MkdirAll(path, 0o755)
	}
	if isMount {
		klog.Infof("%s already mounted", path)
	}
	return isMount, err
}

// unmount and delete mount point, must be idempotent
func (ns *nodeServer) deleteMountPoint(path string) error {
	isMount, err := ns.mounter.IsMountPoint(path)
	if err != nil {
		if os.IsNotExist(err) {
			klog.Infof("%s already deleted", path)
			return nil
		} else if mount.IsCorruptedMnt(err) {
			klog.Warningf("Corrupted mount point detected at %s", path)
			isMount = true
		} else {
			klog.Errorf("Error checking mount point %s: %v", path, err)
			return err
		}
	}

	if isMount {
		err = ns.mounter.Unmount(path)
		if err != nil {
			return err
		}
	}
	return os.RemoveAll(path)
}

func (ns *nodeServer) MakeFile(path string) error {
	// Create file
	newFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0750)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", path, err)
	}
	if err := newFile.Close(); err != nil {
		return fmt.Errorf("failed to close file %s: %w", path, err)
	}
	return nil
}

func getStagingTargetPath(req interface{}) string {
	switch vr := req.(type) {
	case *csi.NodeStageVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodeUnstageVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodePublishVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	default:
		klog.Warningf("invalid request %T", vr)
	}
	return ""
}
