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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"k8s.io/klog"
)

// SpdkNode defines interface for SPDK storage node
//
//   - Info returns node info(rpc url) for debugging purpose
//   - LvStores returns available volume stores(name, size, etc) on that node.
//   - VolumeInfo returns a string map to be passed to client node. Client node
//     needs these info to mount the target. E.g, target IP, service port, nqn.
//   - Create/Delete/Publish/UnpublishVolume per CSI controller service spec.
//
// NOTE: concurrency, idempotency, message ordering
//
// In below text, "implementation" refers to the code implements this interface,
// and "caller" is the code uses the implementation.
//
// Concurrency requirements for implementation and caller:
//   - Implementation should make sure CreateVolume is thread
//     safe. Caller is free to request creating multiple volumes in
//     same volume store concurrently, no data race should happen.
//   - Implementation should make sure
//     PublishVolume/UnpublishVolume/DeleteVolume for *different
//     volumes* thread safe. Caller may issue these requests to
//     *different volumes", in same volume store or not, concurrently.
//   - PublishVolume/UnpublishVolume/DeleteVolume for *same volume* is
//     not thread safe, concurrent access may lead to data
//     race. Caller must serialize these calls to *same volume*,
//     possibly by mutex or message queue per volume.
//   - Implementation should make sure LvStores and VolumeInfo are
//     thread safe, but it doesn't lock the returned resources. It
//     means caller should adopt optimistic concurrency control and
//     retry on specific failures.  E.g, caller calls LvStores and
//     finds a volume store with enough free space, it calls
//     CreateVolume but fails with "not enough space" because another
//     caller may issue similar request at same time. The failed
//     caller may redo above steps(call LvStores, pick volume store,
//     CreateVolume) under this condition, or it can simply fail.
//
// Idempotent requirements for implementation:
// Per CSI spec, it's possible that same request been sent multiple times due to
// issues such as a temporary network failure. Implementation should have basic
// logic to deal with idempotency.
// E.g, ignore publishing an already published volume.
//
// Out of order messages handling for implementation:
// Out of order message may happen in kubernetes CSI framework. E.g, unpublish
// an already deleted volume. The baseline is there should be no code crash or
// data corruption under these conditions. Implementation may try to detect and
// report errors if possible.

// errors deserve special care
var (
	ErrJSONNoSpaceLeft  = errors.New("json: No space left")
	ErrJSONNoSuchDevice = errors.New("json: No such device")

	// internal errors
	ErrVolumeDeleted     = errors.New("volume deleted")
	ErrVolumeUnpublished = errors.New("volume not published")
)

type SpdkNode interface {
	Info() string
	LvStores() ([]LvStore, error)
	VolumeInfo(lvolID string) (map[string]string, error)
	CreateVolume(lvolName, lvsName string, sizeMiB int64) (string, error)
	GetVolume(lvolName, lvsName string) (string, error)
	DeleteVolume(lvolID string) error
	PublishVolume(lvolID string) error
	UnpublishVolume(lvolID string) error
	CreateSnapshot(lvolName, snapshotName string) (string, error)
	DeleteSnapshot(snapshotID string) error
}

// logical volume store
type LvStore struct {
	Name         string
	UUID         string
	TotalSizeMiB int64
	FreeSizeMiB  int64
}

type LvolConnectResp struct {
	Nqn            string `json:"nqn"`
	ReconnectDelay int    `json:"reconnect-delay"`
	NrIoQueues     int    `json:"nr-io-queues"`
	CtrlLossTmo    int    `json:"ctrl-loss-tmo"`
	Port           int    `json:"port"`
	IP             string `json:"ip"`
	Connect        string `json:"connect"`
}

type connectionInfo struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// BDev SPDK block device
type BDev struct {
	Name     string `json:"lvol_name"`
	UUID     string `json:"uuid"`
	LvolSize int64  `json:"size"`
}

// RPCClient holds the connection information to the SimplyBlock Cluster
type RPCClient struct {
	ClusterID     string
	ClusterIP     string
	ClusterSecret string
	HTTPClient    *http.Client
}

// CSIPoolsResp is the response of /pool/get_pools
type CSIPoolsResp struct {
	FreeClusters  int64  `json:"free_clusters"`
	ClusterSize   int64  `json:"cluster_size"`
	TotalClusters int64  `json:"total_data_clusters"`
	Name          string `json:"name"`
	UUID          string `json:"uuid"`
}

// SnapshotResp is the response of /snapshot
type SnapshotResp struct {
	Name         string `json:"snapshot_name"`
	UUID         string `json:"uuid"`
	Size         int64  `json:"size"`
	PoolName     string `json:"pool_name"`
	PoolID       string `json:"pool_uuid"`
	CreatedAt    string `json:"created_at"`
	SourceVolume struct {
		UUID string `json:"id"`
	} `json:"lvol"`
}

// CreateLVolData is the response for /lvol
type CreateVolResp struct {
	LVols []string `json:"lvols"`
}

// ResizeVolReq is the request for /lvol/resize
type ResizeVolReq struct {
	LvolID  string `json:"lvol_id"`
	NewSize int64  `json:"size"`
}

// Error represents SBCLI's common error response
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (client *RPCClient) info() string {
	return client.ClusterID
}

// lvStores returns all available logical volume stores
func (client *RPCClient) lvStores() ([]LvStore, error) {
	var result []CSIPoolsResp

	out, err := client.CallSBCLI("GET", "/pool/get_pools", nil)
	if err != nil {
		return nil, err
	}

	result, ok := out.([]CSIPoolsResp)
	if !ok {
		return nil, fmt.Errorf("failed to convert the response to CSIPoolsResp type. Interface: %v", out)
	}

	lvs := make([]LvStore, len(result))
	for i := range result {
		r := &result[i]
		lvs[i].Name = r.Name
		lvs[i].UUID = r.UUID
		lvs[i].TotalSizeMiB = r.TotalClusters * r.ClusterSize / 1024 / 1024
		lvs[i].FreeSizeMiB = r.FreeClusters * r.ClusterSize / 1024 / 1024
	}

	return lvs, nil
}

// createVolume create a logical volume with simplyblock storage
func (client *RPCClient) createVolume(params *CreateLVolData) (string, error) {
	var lvolID string
	klog.V(5).Info("params", params)

	out, err := client.CallSBCLI("POST", "/lvol", &params)
	if err != nil {
		if errorMatches(err, ErrJSONNoSpaceLeft) {
			err = ErrJSONNoSpaceLeft // may happen in concurrency
		}
		return "", err
	}

	lvolID, ok := out.(string)
	if !ok {
		return "", fmt.Errorf("failed to convert the response to string type. Interface: %v", out)
	}
	return lvolID, err
}

// getVolume gets a volume and return a BDev,, lvsName/lvolName
func (client *RPCClient) getVolume(lvolID string) (*BDev, error) {
	var result []BDev

	out, err := client.CallSBCLI("GET", "/lvol/"+lvolID, nil)
	if err != nil {
		if errorMatches(err, ErrJSONNoSuchDevice) {
			err = ErrJSONNoSuchDevice
		}
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal the response: %w", err)
	}
	err = json.Unmarshal(b, &result)
	if err != nil {
		return nil, err
	}
	return &result[0], err
}

// listVolumes returns all volumes
func (client *RPCClient) listVolumes() ([]*BDev, error) {
	var results []*BDev

	out, err := client.CallSBCLI("GET", "/lvol", nil)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal the response: %w", err)
	}
	err = json.Unmarshal(b, &results)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// getVolumeInfo gets a volume along with its connection info
func (client *RPCClient) getVolumeInfo(lvolID string) (map[string]string, error) {
	var result []*LvolConnectResp

	out, err := client.CallSBCLI("GET", "/lvol/connect/"+lvolID, nil)
	if err != nil {
		klog.Error(err)
		if errorMatches(err, ErrJSONNoSuchDevice) {
			err = ErrJSONNoSuchDevice
		}
		return nil, err
	}

	byteData, err := json.Marshal(out)
	if err != nil {
		klog.Error(err)
		return nil, err
	}
	err = json.Unmarshal(byteData, &result)
	if err != nil {
		return nil, err
	}

	var connections []connectionInfo
	for _, r := range result {
		connections = append(connections, connectionInfo{IP: r.IP, Port: r.Port})
	}

	connectionsData, err := json.Marshal(connections)
	if err != nil {
		klog.Error(err)
		return nil, err
	}

	return map[string]string{
		"name":           lvolID,
		"uuid":           lvolID,
		"nqn":            result[0].Nqn,
		"reconnectDelay": strconv.Itoa(result[0].ReconnectDelay),
		"nrIoQueues":     strconv.Itoa(result[0].NrIoQueues),
		"ctrlLossTmo":    strconv.Itoa(result[0].CtrlLossTmo),
		"model":          lvolID,
		"targetType":     "tcp",
		"connections":    string(connectionsData),
	}, nil
}

// deleteVolume deletes a volume
func (client *RPCClient) deleteVolume(lvolID string) error {
	_, err := client.CallSBCLI("DELETE", "/lvol/"+lvolID, nil)
	if errorMatches(err, ErrJSONNoSuchDevice) {
		err = ErrJSONNoSuchDevice // may happen in concurrency
	}

	return err
}

// resizeVolume resizes a volume
func (client *RPCClient) resizeVolume(lvolID string, size int64) (bool, error) {
	params := ResizeVolReq{
		LvolID:  lvolID,
		NewSize: size,
	}
	var result bool
	out, err := client.CallSBCLI("PUT", "/lvol/resize/"+lvolID, &params)
	if err != nil {
		return false, err
	}
	result, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("failed to convert the response to bool type. Interface: %v", out)
	}
	return result, nil
}

// cloneSnapshot clones a snapshot
func (client *RPCClient) cloneSnapshot(snapshotID, cloneName, newSize string) (string, error) {
	params := struct {
		SnapshotID string `json:"snapshot_id"`
		CloneName  string `json:"clone_name"`
		//	NewSize    string `json:"new_size"`
	}{
		SnapshotID: snapshotID,
		CloneName:  cloneName,
		//	NewSize:    newSize,
	}

	klog.V(5).Infof("cloned volume size: %s", newSize)

	var lvolID string
	out, err := client.CallSBCLI("POST", "/snapshot/clone", &params)
	if err != nil {
		if errorMatches(err, ErrJSONNoSpaceLeft) {
			err = ErrJSONNoSpaceLeft // may happen in concurrency
		}
		return "", err
	}

	lvolID, ok := out.(string)
	if !ok {
		return "", fmt.Errorf("failed to convert the response to string type. Interface: %v", out)
	}
	return lvolID, err
}

// listSnapshots returns all snapshots
func (client *RPCClient) listSnapshots() ([]*SnapshotResp, error) {
	var results []*SnapshotResp

	out, err := client.CallSBCLI("GET", "/snapshot", nil)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal the response: %w", err)
	}
	err = json.Unmarshal(b, &results)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// snapshot creates a snapshot
func (client *RPCClient) snapshot(lvolID, snapShotName string) (string, error) {
	params := struct {
		LvolName     string `json:"lvol_id"`
		SnapShotName string `json:"snapshot_name"`
	}{
		LvolName:     lvolID,
		SnapShotName: snapShotName,
	}
	var snapshotID string
	out, err := client.CallSBCLI("POST", "/snapshot", &params)
	if err != nil {
		if errorMatches(err, ErrJSONNoSpaceLeft) {
			err = ErrJSONNoSpaceLeft // may happen in concurrency
		}
		return "", err
	}

	snapshotID, ok := out.(string)
	if !ok {
		return "", fmt.Errorf("failed to convert the response to string type. Interface: %v", out)
	}
	return snapshotID, err
}

// deleteSnapshot deletes a snapshot
func (client *RPCClient) deleteSnapshot(snapshotID string) error {
	_, err := client.CallSBCLI("DELETE", "/snapshot/"+snapshotID, nil)

	if errorMatches(err, ErrJSONNoSuchDevice) {
		err = ErrJSONNoSuchDevice // may happen in concurrency
	}

	return err
}

// CallSBCLI is a generic function to call the SimplyBlock API
func (client *RPCClient) CallSBCLI(method, path string, args interface{}) (interface{}, error) {
	data := []byte(`{}`)
	var err error

	if args != nil {
		data, err = json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
	} else {
		data = nil
	}

	requestURL := fmt.Sprintf("%s/%s", client.ClusterIP, path)
	klog.Infof("Calling Simplyblock API: Method: %s: RequestURL: %s: Body: %s\n", method, requestURL, string(data))
	req, err := http.NewRequest(method, requestURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}

	authHeader := fmt.Sprintf("%s %s", client.ClusterID, client.ClusterSecret)

	req.Header.Add("Authorization", authHeader)
	req.Header.Add("cluster", client.ClusterID)
	req.Header.Add("secret", client.ClusterSecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}

	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return nil, fmt.Errorf("%s: HTTP error code: %d", method, resp.StatusCode)
	}

	var response struct {
		Result  any    `json:"result"`
		Results any    `json:"results"`
		Error   string `json:"error"`
	}

	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return nil, fmt.Errorf("%s: HTTP error code: %d Error: %w", method, resp.StatusCode, err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%s: HTTP error code: %d Error: %s", method, resp.StatusCode, response.Error)
	}

	if response.Result != nil {
		return response.Result, nil
	}
	return response.Results, nil
}

// errorMatches checks if the error message from the full error
func errorMatches(errFull, errJSON error) bool {
	if errFull == nil {
		return false
	}
	strFull := strings.ToLower(errFull.Error())
	strJSON := strings.ToLower(errJSON.Error())
	strJSON = strings.TrimPrefix(strJSON, "json:")
	strJSON = strings.TrimSpace(strJSON)
	return strings.Contains(strFull, strJSON)
}
