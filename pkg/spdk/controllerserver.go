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
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	csicommon "github.com/spdk/spdk-csi/pkg/csi-common"
	"github.com/spdk/spdk-csi/pkg/util"
)

// var errVolumeInCreation = status.Error(codes.Internal, "volume in creation")
const (
	CSIStorageBaseKey      = "csi.storage.k8s.io/pvc"
	CSIStorageNameKey      = CSIStorageBaseKey + "/name"
	CSIStorageNamespaceKey = CSIStorageBaseKey + "/namespace"
)

type controllerServer struct {
	*csicommon.DefaultControllerServer
	volumeLocks *util.VolumeLocks
	spdkNode    *util.NodeNVMf
}

type spdkVolume struct {
	lvolID   string
	poolName string
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	volumeID := req.GetName()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	csiVolume, err := cs.createVolume(ctx, req)
	if err != nil {
		klog.Errorf("failed to create volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	volumeInfo, err := cs.publishVolume(csiVolume.GetVolumeId())
	if err != nil {
		klog.Errorf("failed to publish volume, volumeID: %s err: %v", volumeID, err)
		cs.deleteVolume(csiVolume.GetVolumeId()) //nolint:errcheck // we can do little
		return nil, status.Error(codes.Internal, err.Error())
	}

	// copy volume info. node needs these info to contact target(ip, port, nqn, ...)
	if csiVolume.VolumeContext == nil {
		csiVolume.VolumeContext = volumeInfo
	} else {
		for k, v := range volumeInfo {
			csiVolume.VolumeContext[k] = v
		}
	}

	if volType, ok := req.GetParameters()["type"]; ok {
		csiVolume.VolumeContext["targetType"] = volType
	}

	return &csi.CreateVolumeResponse{Volume: csiVolume}, nil
}

func (cs *controllerServer) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()
	// no harm if volume already unpublished
	err := cs.unpublishVolume(volumeID)
	switch {
	case errors.Is(err, util.ErrVolumeUnpublished):
		// unpublished but not deleted in last request?
		klog.Warningf("volume not published: %s", volumeID)
	case errors.Is(err, util.ErrVolumeDeleted):
		// deleted in previous request?
		klog.Warningf("volume already deleted: %s", volumeID)
	case err != nil:
		klog.Errorf("failed to unpublish volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	// no harm if volume already deleted
	err = cs.deleteVolume(volumeID)
	if errors.Is(err, util.ErrJSONNoSuchDevice) {
		// deleted in previous request?
		klog.Warningf("volume not exists: %s", volumeID)
	} else if err != nil {
		klog.Errorf("failed to delete volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	// make sure we support all requested caps
	for _, cap := range req.GetVolumeCapabilities() {
		supported := false
		for _, accessMode := range cs.Driver.GetVolumeCapabilityAccessModes() {
			if cap.GetAccessMode().GetMode() == accessMode.GetMode() {
				supported = true
				break
			}
		}
		if !supported {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

func (cs *controllerServer) CreateSnapshot(_ context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	volumeID := req.GetSourceVolumeId()
	klog.Infof("CreateSnapshot : volumeID=%s", volumeID)
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	snapshotName := req.GetName()
	klog.Infof("CreateSnapshot : snapshotName=%s", snapshotName)
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		klog.Errorf("failed to get spdk volume, volumeID: %s err: %v", volumeID, err)
		return nil, err
	}
	snapshotID, err := cs.spdkNode.CreateSnapshot(spdkVol.lvolID, snapshotName)
	klog.Infof("CreateSnapshot : snapshotID=%s", snapshotID)
	if err != nil {
		klog.Errorf("failed to create snapshot, volumeID: %s snapshotName: %s err: %v", volumeID, snapshotName, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	volSize, err := cs.spdkNode.GetVolumeSize(spdkVol.lvolID)
	klog.Infof("CreateSnapshot : volSize=%s", volSize)
	if err != nil {
		klog.Errorf("failed to get volume info, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	size, err := strconv.ParseInt(volSize, 10, 64)
	if err != nil {
		klog.Errorf("failed to parse volume size, size: %s err: %v", volSize, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	creationTime := timestamppb.Now()
	snapshotData := csi.Snapshot{
		SizeBytes:      size,
		SnapshotId:     snapshotID,
		SourceVolumeId: spdkVol.lvolID,
		CreationTime:   creationTime,
		ReadyToUse:     true,
	}

	return &csi.CreateSnapshotResponse{
		Snapshot: &snapshotData,
	}, nil
}

func (cs *controllerServer) DeleteSnapshot(_ context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	snapshotID := req.GetSnapshotId()
	klog.Infof("snapshotID=%s", snapshotID)
	unlock := cs.volumeLocks.Lock(snapshotID)
	defer unlock()

	klog.Infof("Deleting Snapshot : snapshotID=%s", snapshotID)

	err := cs.spdkNode.DeleteSnapshot(snapshotID)
	if err != nil {
		klog.Errorf("failed to delete snapshot, snapshotID: %s err: %v", snapshotID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.DeleteSnapshotResponse{}, nil
}

func getIntParameter(params map[string]string, key string, defaultValue int) (int, error) {
	if valueStr, exists := params[key]; exists {
		value, err := strconv.Atoi(valueStr)
		if err != nil {
			return 0, fmt.Errorf("error converting %s: %w", key, err)
		}
		return value, nil
	}
	return defaultValue, nil
}

func getBoolParameter(params map[string]string, key string) bool {
	valueStr, exists := params[key]
	return exists && (valueStr == "true" || valueStr == "True")
}

func prepareCreateVolumeReq(ctx context.Context, req *csi.CreateVolumeRequest, sizeMiB int64) (*util.CreateLVolData, error) {
	params := req.GetParameters()

	distrNdcs, err := getIntParameter(params, "distr_ndcs", 1)
	if err != nil {
		return nil, err
	}
	distrNpcs, err := getIntParameter(params, "distr_npcs", 1)
	if err != nil {
		return nil, err
	}

	priorClass, err := getIntParameter(params, "lvol_priority_class", 0)
	if err != nil {
		return nil, err
	}

	compression := getBoolParameter(params, "compression")
	encryption := getBoolParameter(params, "encryption")

	pvcName, pvcNameSelected := params[CSIStorageNameKey]
	pvcNamespace, pvcNamespaceSelected := params[CSIStorageNamespaceKey]

	var cryptoKey1 string
	var cryptoKey2 string

	if encryption {
		if pvcNameSelected && pvcNamespaceSelected {
			cryptoKey1, cryptoKey2, err = GetCryptoKeys(ctx, pvcName, pvcNamespace)
			if err != nil {
				klog.Errorf("failed to get crypto keys: %v", err)
				return nil, fmt.Errorf("failed to get crypto keys: %w", err)
			}
			if cryptoKey1 == "" || cryptoKey2 == "" {
				return nil, errors.New("encryption is requested but crypto keys are missing")
			}
		} else {
			return nil, errors.New("encryption requested but PVC name or namespace is not provided")
		}
	}

	hostID, err := getHostIDAnnotation(ctx, pvcName, pvcNamespace)
	if err != nil {
		return nil, err
	}

	createVolReq := util.CreateLVolData{
		LvolName:    req.GetName(),
		Size:        fmt.Sprintf("%dM", sizeMiB),
		LvsName:     params["pool_name"],
		MaxRWIOPS:   params["qos_rw_iops"],
		MaxRWmBytes: params["qos_rw_mbytes"],
		MaxRmBytes:  params["qos_r_mbytes"],
		MaxWmBytes:  params["qos_w_mbytes"],
		MaxSize:     params["max_size"],
		PriorClass:  priorClass,
		Compression: compression,
		Encryption:  encryption,
		DistNdcs:    distrNdcs,
		DistNpcs:    distrNpcs,
		CryptoKey1:  cryptoKey1,
		CryptoKey2:  cryptoKey2,
		HostID:      hostID,
	}
	return &createVolReq, nil
}

func (cs *controllerServer) getExistingVolume(name, poolName string, vol *csi.Volume) (*csi.Volume, error) {
	volumeID, err := cs.spdkNode.GetVolume(name, poolName)
	if err == nil {
		vol.VolumeId = fmt.Sprintf("%s:%s", poolName, volumeID)
		klog.V(5).Info("volume already exists", vol.GetVolumeId())
		return vol, nil
	}
	return nil, err
}

func (cs *controllerServer) createVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.Volume, error) {
	size := req.GetCapacityRange().GetRequiredBytes()
	if size == 0 {
		klog.Warningln("invalid volume size, resize to 1G")
		size = 1024 * 1024 * 1024
	}
	sizeMiB := util.ToMiB(size)
	vol := csi.Volume{
		CapacityBytes: sizeMiB * 1024 * 1024,
		VolumeContext: req.GetParameters(),
		ContentSource: req.GetVolumeContentSource(),
	}

	klog.V(5).Info("provisioning volume from SDK node..")
	poolName := req.GetParameters()["pool_name"]
	existingVolume, err := cs.getExistingVolume(req.GetName(), poolName, &vol)
	if err == nil {
		return existingVolume, nil
	}

	if req.GetVolumeContentSource() != nil {
		clonedVolume, clonedErr := cs.handleVolumeContentSource(req, poolName, &vol, sizeMiB)
		if clonedErr != nil {
			return nil, clonedErr
		}
		if clonedVolume != nil {
			return clonedVolume, nil
		}
	}

	createVolReq, err := prepareCreateVolumeReq(ctx, req, sizeMiB)
	if err != nil {
		return nil, err
	}

	volumeID, err := cs.spdkNode.CreateVolume(createVolReq)
	if err != nil {
		klog.Errorf("error creating simplyBlock volume: %v", err)
		return nil, err
	}
	vol.VolumeId = fmt.Sprintf("%s:%s", poolName, volumeID)
	klog.V(5).Info("successfully created volume from Simplyblock with Volume ID: ", vol.GetVolumeId())

	return &vol, nil
}

func getSPDKVol(csiVolumeID string) (*spdkVolume, error) {
	// extract spdkNodeName and spdkLvolID from csiVolumeID
	// csiVolumeID: node001:8e2dcb9d-3a79-4362-965e-fdb0cd3f4b8d
	// spdkNodeName: node001
	// spdklvolID: 8e2dcb9d-3a79-4362-965e-fdb0cd3f4b8d

	ids := strings.Split(csiVolumeID, ":")
	if len(ids) == 2 {
		return &spdkVolume{
			poolName: ids[0],
			lvolID:   ids[1],
		}, nil
	}
	return nil, fmt.Errorf("missing poolName in volume: %s", csiVolumeID)
}

func (cs *controllerServer) publishVolume(volumeID string) (map[string]string, error) {
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		return nil, err
	}
	err = cs.spdkNode.PublishVolume(spdkVol.lvolID)
	if err != nil {
		return nil, err
	}

	volumeInfo, err := cs.spdkNode.VolumeInfo(spdkVol.lvolID)
	if err != nil {
		cs.unpublishVolume(volumeID) //nolint:errcheck // we can do little
		return nil, err
	}
	return volumeInfo, nil
}

func (cs *controllerServer) deleteVolume(volumeID string) error {
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		return err
	}
	return cs.spdkNode.DeleteVolume(spdkVol.lvolID)
}

func (cs *controllerServer) unpublishVolume(volumeID string) error {
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		return err
	}
	return cs.spdkNode.UnpublishVolume(spdkVol.lvolID)
}

func (cs *controllerServer) ControllerExpandVolume(_ context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	updatedSize := req.GetCapacityRange().GetRequiredBytes()
	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		return nil, err
	}
	_, err = cs.spdkNode.ResizeVolume(spdkVol.lvolID, updatedSize)
	if err != nil {
		klog.Errorf("failed to resize lvol, LVolID: %s err: %v", spdkVol.lvolID, err)
		return nil, err
	}
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         updatedSize,
		NodeExpansionRequired: true,
	}, nil
}

func (cs *controllerServer) ListSnapshots(_ context.Context, _ *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	entries, err := cs.spdkNode.ListSnapshots()
	if err != nil {
		return nil, err
	}
	var vca []*csi.ListSnapshotsResponse_Entry
	for _, entry := range entries {
		sz, err := strconv.ParseInt(entry.Size, 10, 64)
		if err != nil {
			return nil, err
		}
		dt, err := strconv.ParseInt(entry.CreatedAt, 10, 64)
		if err != nil {
			return nil, err
		}
		snapshotData := &csi.Snapshot{
			SizeBytes:      sz,
			SnapshotId:     fmt.Sprintf("%s:%s", entry.PoolName, entry.UUID),
			SourceVolumeId: entry.SourceUUID,
			CreationTime: &timestamppb.Timestamp{
				Seconds: dt,
			},
			ReadyToUse: true,
		}

		responseData := &csi.ListSnapshotsResponse_Entry{
			Snapshot: snapshotData,
		}

		vca = append(vca, responseData)
	}

	return &csi.ListSnapshotsResponse{
		Entries: vca,
	}, nil
}

func NewsimplyBlockClient() (*util.NodeNVMf, error) {
	// get spdk node configs, see deploy/kubernetes/config-map.yaml
	var config struct {
		Simplybk struct {
			UUID string `json:"uuid"`
			IP   string `json:"ip"`
		} `json:"simplybk"`
	}
	configFile := util.FromEnv("SPDKCSI_CONFIG", "/etc/spdkcsi-config/config.json")
	err := util.ParseJSONFile(configFile, &config)
	if err != nil {
		return nil, err
	}

	var secret struct {
		Simplybk struct {
			Secret string `json:"secret"`
		} `json:"simplybk"`
	}
	secretFile := util.FromEnv("SPDKCSI_SECRET", "/etc/spdkcsi-secret/secret.json")
	err = util.ParseJSONFile(secretFile, &secret)
	if err != nil {
		return nil, err
	}
	klog.Infof("spdk node created: url=%s", config.Simplybk.IP)

	return util.NewNVMf(config.Simplybk.UUID, config.Simplybk.IP, secret.Simplybk.Secret), nil
}

// func (cs *controllerServer) ListVolumes(_ context.Context, _ *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
// 	volumes := []*csi.ListVolumesResponse_Entry{}

// 	volumeIDs, err := cs.spdkNode.ListVolumes()
// 	if err != nil {
// 		klog.Errorf("failed to list volumes: %v", err)
// 		return nil, status.Error(codes.Internal, err.Error())
// 	}

// 	for _, volumeID := range volumeIDs {
// 		volumeInfo, err := cs.spdkNode.VolumeInfo(volumeID.UUID)
// 		if err != nil {
// 			klog.Errorf("failed to get volume info for volume %s: %v", volumeID.UUID, err)
// 			return nil, status.Error(codes.NotFound, err.Error())
// 		}
// 		volume := &csi.Volume{
// 			VolumeId:      volumeID.UUID,
// 			VolumeContext: volumeInfo,
// 		}

// 		volumes = append(volumes, &csi.ListVolumesResponse_Entry{
// 			Volume: volume,
// 		})
// 	}

// 	return &csi.ListVolumesResponse{
// 		Entries: volumes,
// 	}, nil
// }

//	func (cs *controllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
//		return nil, status.Error(codes.Unimplemented, "")
//	}

func (cs *controllerServer) ControllerGetVolume(_ context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	spdkVol, err := getSPDKVol(volumeID)
	if err != nil {
		return nil, err
	}

	volumeInfo, err := cs.spdkNode.VolumeInfo(spdkVol.lvolID)
	if err != nil {
		klog.Errorf("failed to get spdkVol for %s: %v", volumeID, err)

		return &csi.ControllerGetVolumeResponse{
			Volume: &csi.Volume{
				VolumeId: volumeID,
			},
			Status: &csi.ControllerGetVolumeResponse_VolumeStatus{
				VolumeCondition: &csi.VolumeCondition{
					Abnormal: true,
					Message:  err.Error(),
				},
			},
		}, nil
	}

	volume := &csi.Volume{
		VolumeId:      spdkVol.lvolID,
		VolumeContext: volumeInfo,
	}

	return &csi.ControllerGetVolumeResponse{
		Volume: volume,
		Status: &csi.ControllerGetVolumeResponse_VolumeStatus{
			VolumeCondition: &csi.VolumeCondition{
				Abnormal: false,
				Message:  "",
			},
		},
	}, nil
}

func newControllerServer(d *csicommon.CSIDriver) (*controllerServer, error) {
	server := controllerServer{
		DefaultControllerServer: csicommon.NewDefaultControllerServer(d),
		volumeLocks:             util.NewVolumeLocks(),
	}

	spdkNode, err := NewsimplyBlockClient()
	if err != nil {
		klog.Errorf("failed to create spdk node %v", err.Error())
		return nil, errors.New("no valid spdk node found")
	}

	server.spdkNode = spdkNode
	return &server, nil

	// create spdk nodes
	// for i := range config.Nodes {
	// 	node := &config.Nodes[i]
	// 	tokenFound := false
	// 	// find secret per node
	// 	for j := range secret.Tokens {
	// 		token := &secret.Tokens[j]
	// 		if token.Name == node.Name {
	// 			tokenFound = true
	// 			spdkNode, err := util.NewSpdkNode(node.URL, token.UserName, token.Password, node.TargetType, node.TargetAddr)
	// 			if err != nil {
	// 				klog.Errorf("failed to create spdk node %s: %s", node.Name, err.Error())
	// 			} else {
	// 				klog.Infof("spdk node created: name=%s, url=%s", node.Name, node.URL)
	// 				server.spdkNodes[node.Name] = spdkNode
	// 			}
	// 			break
	// 		}
	// 	}
	// 	if !tokenFound {
	// 		klog.Errorf("failed to find secret for spdk node %s", node.Name)
	// 	}
	// }
	// if len(server.spdkNodes) == 0 {
	// 	return nil, fmt.Errorf("no valid spdk node found")
	// }

	// return &server, nil
}

func (cs *controllerServer) handleVolumeContentSource(req *csi.CreateVolumeRequest, poolName string, vol *csi.Volume, sizeMiB int64) (*csi.Volume, error) {
	volumeSource := req.GetVolumeContentSource()
	switch volumeSource.GetType().(type) {
	case *csi.VolumeContentSource_Snapshot:
		return cs.handleSnapshotSource(volumeSource.GetSnapshot(), req, poolName, vol, sizeMiB)
	case *csi.VolumeContentSource_Volume:
		return cs.handleVolumeSource(volumeSource.GetVolume(), req, poolName, vol, sizeMiB)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "%v not a proper volume source", volumeSource)
	}
}

func (cs *controllerServer) handleSnapshotSource(snapshot *csi.VolumeContentSource_SnapshotSource, req *csi.CreateVolumeRequest, poolName string, vol *csi.Volume, sizeMiB int64) (*csi.Volume, error) {
	if snapshot == nil {
		return nil, nil
	}
	snapshotID := snapshot.GetSnapshotId()
	klog.Infof("CreateSnapshot : snapshotID=%s", snapshotID)
	snapshotName := req.GetName()
	newSize := fmt.Sprintf("%dM", sizeMiB)
	volumeID, err := cs.spdkNode.CloneSnapshot(snapshotID, snapshotName, newSize)
	if err != nil {
		klog.Errorf("error creating simplyBlock volume: %v", err)
		return nil, err
	}
	vol.VolumeId = fmt.Sprintf("%s:%s", poolName, volumeID)
	klog.V(5).Info("successfully Restored Snapshot from Simplyblock with Volume ID: ", vol.GetVolumeId())

	return vol, nil
}

func (cs *controllerServer) handleVolumeSource(srcVolume *csi.VolumeContentSource_VolumeSource, req *csi.CreateVolumeRequest, poolName string, vol *csi.Volume, sizeMiB int64) (*csi.Volume, error) {
	if srcVolume == nil {
		return nil, nil
	}
	srcVolumeID := srcVolume.GetVolumeId()

	klog.Infof("srcVolumeID=%s", srcVolumeID)

	snapshotName := req.GetName()
	spdkVol, err := getSPDKVol(srcVolumeID)
	if err != nil {
		klog.Errorf("failed to get spdk volume, srcVolumeID: %s err: %v", srcVolumeID, err)
		return nil, err
	}
	klog.Infof("CreateSnapshot : poolName=%s", poolName)
	snapshotID, err := cs.spdkNode.CreateSnapshot(spdkVol.lvolID, snapshotName)
	klog.Infof("CreateSnapshot : snapshotID=%s", snapshotID)
	if err != nil {
		klog.Errorf("failed to create snapshot, srcVolumeID: %s snapshotName: %s err: %v", srcVolumeID, snapshotName, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	newSize := fmt.Sprintf("%dM", sizeMiB)
	klog.Infof("CloneSnapshot : snapshotName=%s", snapshotName)
	volumeID, err := cs.spdkNode.CloneSnapshot(snapshotID, snapshotName, newSize)
	if err != nil {
		klog.Errorf("error creating simplyBlock volume: %v", err)
		return nil, err
	}
	vol.VolumeId = fmt.Sprintf("%s:%s", poolName, volumeID)
	klog.V(5).Info("successfully created clonesnapshot volume from Simplyblock with Volume ID: ", vol.GetVolumeId())

	return vol, nil
}

func GetCryptoKeys(ctx context.Context, pvcName, pvcNamespace string) (cryptoKey1, cryptoKey2 string, err error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Errorf("failed to get in-cluster config: %v", err)
		return "", "", fmt.Errorf("could not get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Errorf("failed to create clientset: %v", err)
		return "", "", fmt.Errorf("could not create clientset: %w", err)
	}

	pvc, err := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("failed to get PVC %s in namespace %s: %v", pvcName, pvcNamespace, err)
		return "", "", fmt.Errorf("could not get PVC %s in namespace %s: %w", pvcName, pvcNamespace, err)
	}

	secretName := pvc.ObjectMeta.Annotations["simplybk/secret-name"]
	secretNamespace := pvc.ObjectMeta.Annotations["simplybk/secret-namespace"]

	secret, err := clientset.CoreV1().Secrets(secretNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("failed to get secret %s in namespace %s: %v", secretName, secretNamespace, err)
		return "", "", fmt.Errorf("could not get secret %s in namespace %s: %w", secretName, secretNamespace, err)
	}

	key1, ok := secret.Data["crypto_key1"]
	if !ok {
		return "", "", fmt.Errorf("crypto_key1 not found in secret %s", secretName)
	}
	key2, ok := secret.Data["crypto_key2"]
	if !ok {
		return "", "", fmt.Errorf("crypto_key2 not found in secret %s", secretName)
	}

	return strings.TrimSpace(string(key1)), strings.TrimSpace(string(key2)), nil
}

func getHostIDAnnotation(ctx context.Context, pvcName, pvcNamespace string) (string, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Errorf("failed to get in-cluster config: %v", err)
		return "", fmt.Errorf("could not get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Errorf("failed to create clientset: %v", err)
		return "", fmt.Errorf("could not create clientset: %w", err)
	}

	pvc, err := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("failed to get PVC %s in namespace %s: %v", pvcName, pvcNamespace, err)
		return "", fmt.Errorf("could not get PVC %s in namespace %s: %w", pvcName, pvcNamespace, err)
	}

	hostID, ok := pvc.ObjectMeta.Annotations["simplybk/host-id"]
	if !ok {
		return "", nil
	}

	return hostID, nil
}
