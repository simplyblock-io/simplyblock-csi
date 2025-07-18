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

package util

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"k8s.io/klog"
)

type NodeNVMf struct {
	Client *RPCClient
}

// NewNVMf creates a new NVMf client
func NewNVMf(clusterID, clusterIP, clusterSecret string) *NodeNVMf {
	client := RPCClient{
		HTTPClient:    &http.Client{Timeout: cfgRPCTimeoutSeconds * time.Second},
		ClusterID:     clusterID,
		ClusterIP:     clusterIP,
		ClusterSecret: clusterSecret,
	}
	return &NodeNVMf{
		Client: &client,
	}
}

func (node *NodeNVMf) Info() string {
	return node.Client.info()
}

func (node *NodeNVMf) LvStores() ([]LvStore, error) {
	return node.Client.lvStores()
}

// VolumeInfo returns a string:string map containing information necessary
// for CSI node(initiator) to connect to this target and identify the disk.
func (node *NodeNVMf) VolumeInfo(lvolID string) (map[string]string, error) {
	lvol, err := node.Client.getVolumeInfo(lvolID)
	if err != nil {
		return nil, err
	}

	return lvol, nil
}

// CreateLVolData is the data structure for creating a logical volume
type CreateLVolData struct {
	LvolName    string `json:"name"`
	Size        string `json:"size"`
	LvsName     string `json:"pool"`
	Compression bool   `json:"comp"`
	Encryption  bool   `json:"crypto"`
	MaxRWIOPS   string `json:"max_rw_iops"`
	MaxRWmBytes string `json:"max_rw_mbytes"`
	MaxRmBytes  string `json:"max_r_mbytes"`
	MaxWmBytes  string `json:"max_w_mbytes"`
	MaxSize     string `json:"max_size"`
	DistNdcs    int    `json:"distr_ndcs"`
	DistNpcs    int    `json:"distr_npcs"`
	PriorClass  int    `json:"lvol_priority_class"`
	CryptoKey1  string `json:"crypto_key1"`
	CryptoKey2  string `json:"crypto_key2"`
	HostID      string `json:"host_id"`
	LvolID      string `json:"namespace"`
	PvcName     string `json:"pvc_name"`
}

// CreateVolume creates a logical volume and returns volume ID
func (node *NodeNVMf) CreateVolume(params *CreateLVolData) (string, error) {
	lvolID, err := node.Client.createVolume(params)
	if err != nil {
		return "", err
	}
	klog.V(5).Infof("volume created: %s", lvolID)
	return lvolID, nil
}

// GetVolume returns the volume id of the given volume name and lvstore name. return error if not found.
func (node *NodeNVMf) GetVolume(lvolName, poolName string) (string, error) {
	lvol, err := node.Client.getVolume(fmt.Sprintf("%s/%s", poolName, lvolName))
	if err != nil {
		return "", err
	}
	return lvol.UUID, err
}

// GetVolumeSize returns the size of the volume
func (node *NodeNVMf) GetVolumeSize(lvolID string) (string, error) {
	lvol, err := node.Client.getVolume(lvolID)
	if err != nil {
		return "", err
	}

	size := strconv.FormatInt(lvol.LvolSize, 10)
	return size, err
}

// ListVolumes returns a list of volumes
func (node *NodeNVMf) ListVolumes() ([]*BDev, error) {
	return node.Client.listVolumes()
}

// ResizeVolume resizes a volume
func (node *NodeNVMf) ResizeVolume(lvolID string, newSize int64) (bool, error) {
	return node.Client.resizeVolume(lvolID, newSize)
}

// ListSnapshots returns a list of snapshots
func (node *NodeNVMf) ListSnapshots() ([]*SnapshotResp, error) {
	return node.Client.listSnapshots()
}

// CloneSnapshot clones a snapshot to a new volume
func (node *NodeNVMf) CloneSnapshot(snapshotID, cloneName, newSize, pvcName string) (string, error) {
	lvolID, err := node.Client.cloneSnapshot(snapshotID, cloneName, newSize, pvcName)
	if err != nil {
		return "", err
	}
	klog.V(5).Infof("snapshot cloned: %s", lvolID)
	return lvolID, nil
}

// CreateSnapshot creates a snapshot of a volume
func (node *NodeNVMf) CreateSnapshot(lvolID, snapshotName string) (string, error) {
	snapshotID, err := node.Client.snapshot(lvolID, snapshotName)
	if err != nil {
		return "", err
	}
	snapshotID = fmt.Sprintf("%s:%s", node.Client.ClusterID, snapshotID)
	klog.V(5).Infof("snapshot created: %s", snapshotID)
	return snapshotID, nil
}

// DeleteVolume deletes a volume
func (node *NodeNVMf) DeleteVolume(lvolID string) error {
	err := node.Client.deleteVolume(lvolID)
	if err != nil {
		return err
	}
	klog.V(5).Infof("volume deleted: %s", lvolID)
	return nil
}

// DeleteSnapshot deletes a snapshot
func (node *NodeNVMf) DeleteSnapshot(snapshotID string) error {
	err := node.Client.deleteSnapshot(snapshotID)
	if err != nil {
		return err
	}
	klog.V(5).Infof("snapshot deleted: %s", snapshotID)
	return nil
}

// PublishVolume exports a volume through NVMf target
func (node *NodeNVMf) PublishVolume(lvolID string) error {
	_, err := node.Client.CallSBCLI("GET", "/lvol/"+lvolID, nil)
	if err != nil {
		return err
	}
	klog.V(5).Infof("volume published: %s", lvolID)
	return nil
}

// UnpublishVolume unexports a volume through NVMf target
func (node *NodeNVMf) UnpublishVolume(lvolID string) error {
	_, err := node.Client.CallSBCLI("GET", "/lvol/"+lvolID, nil)
	if err != nil {
		return err
	}

	klog.V(5).Infof("volume unpublished: %s", lvolID)
	return nil
}
