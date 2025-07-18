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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog"
)

const (
	// DevDiskByID is the path to the device file under /dev/disk/by-id
	DevDiskByID = "/dev/disk/by-id/*%s*"

	// TargetTypeNVMf is the target type for NVMe over Fabrics
	TargetTypeNVMf = "tcp"

	// TargetTypeISCSI is the target type for cache
	TargetTypeCache = "cache"
)

// SpdkCsiInitiator defines interface for NVMeoF/iSCSI initiator
//   - Connect initiates target connection and returns local block device filename
//     e.g., /dev/disk/by-id/nvme-SPDK_Controller1_SPDK00000000000001
//   - Disconnect terminates target connection
//   - Caller(node service) should serialize calls to same initiator
//   - Implementation should be idempotent to duplicated requests
type SpdkCsiInitiator interface {
	Connect() (string, error)
	Disconnect() error
}

// initiatorNVMf is an implementation of NVMf tcp initiator
type initiatorNVMf struct {
	targetType     string
	connections    []connectionInfo
	nqn            string
	reconnectDelay string
	nrIoQueues     string
	ctrlLossTmo    string
	model          string
	nsId           string
}

// initiatorCache is an implementation of NVMf cache initiator
type initiatorCache struct {
	lvol   string
	model  string
	client RPCClient // TODO: support multi cluster for cache
}

type cachingNodeList struct {
	Hostname string `json:"hostname"`
	UUID     string `json:"id"`
}

type lVolCachingNodeConnect struct {
	LvolID string `json:"lvol_id"`
}

type path struct {
	Name      string `json:"Name"`
	Transport string `json:"Transport"`
	Address   string `json:"Address"`
	State     string `json:"State"`
	ANAState  string `json:"ANAState"`
}

type subsystem struct {
	Name  string `json:"Name"`
	NQN   string `json:"NQN"`
	Paths []path `json:"Paths"`
}

type subsystemResponse struct {
	Subsystems []subsystem `json:"Subsystems"`
}

type NodeInfo struct {
	NodeID string   `json:"node_id"`
	Nodes  []string `json:"nodes"`
	Status string   `json:"status"`
}

type nvmeDeviceInfo struct {
	devicePath   string
	serialNumber string
}

// clusterConfig represents the Kubernetes secret structure
type ClusterConfig struct {
	ClusterID       string `json:"cluster_id"`
	ClusterEndpoint string `json:"cluster_endpoint"`
	ClusterSecret   string `json:"cluster_secret"`
}

type ClustersInfo struct {
	Clusters []ClusterConfig `json:"clusters"`
}

// NewsimplyBlockClient create a new Simplyblock client
// should be called for every CSI driver operation
func NewsimplyBlockClient(clusterID string) (*NodeNVMf, error) {
	secretFile := FromEnv("SPDKCSI_SECRET", "/etc/spdkcsi-secret/secret.json")
	var clusters ClustersInfo
	err := ParseJSONFile(secretFile, &clusters)
	if err != nil {
		return nil, fmt.Errorf("failed to parse secret file: %w", err)
	}

	var clusterConfig *ClusterConfig
	for _, cluster := range clusters.Clusters {
		if cluster.ClusterID == clusterID {
			clusterConfig = &cluster
			break
		}
	}

	if clusterConfig == nil {
		return nil, fmt.Errorf("failed to find secret for clusterID %s", clusterID)
	}

	if clusterConfig.ClusterEndpoint == "" || clusterConfig.ClusterSecret == "" {
		return nil, fmt.Errorf("invalid cluster configuration for clusterID %s", clusterID)
	}

	// Log and return the newly created Simplyblock client.
	klog.Infof("Simplyblock client created for ClusterID:%s, Endpoint:%s",
		clusterConfig.ClusterID,
		clusterConfig.ClusterEndpoint,
	)
	return NewNVMf(clusterID, clusterConfig.ClusterEndpoint, clusterConfig.ClusterSecret), nil
}

// NewSpdkCsiInitiator creates a new SpdkCsiInitiator based on the target type
func NewSpdkCsiInitiator(volumeContext map[string]string) (SpdkCsiInitiator, error) {
	targetType := strings.ToLower(volumeContext["targetType"])
	switch targetType {
	case TargetTypeNVMf:
		var connections []connectionInfo

		err := json.Unmarshal([]byte(volumeContext["connections"]), &connections)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshall connections. Error: %v", err.Error())
		}

		return &initiatorNVMf{
			targetType:     volumeContext["targetType"],
			connections:    connections,
			nqn:            volumeContext["nqn"],
			reconnectDelay: volumeContext["reconnectDelay"],
			nrIoQueues:     volumeContext["nrIoQueues"],
			ctrlLossTmo:    volumeContext["ctrlLossTmo"],
			model:          volumeContext["model"],
			nsId:           volumeContext["nsId"],
		}, nil

	case "cache":
		return &initiatorCache{
			lvol:  volumeContext["uuid"],
			model: volumeContext["model"],
		}, nil

	default:
		return nil, fmt.Errorf("unknown initiator: %s", targetType)
	}
}

func (cache *initiatorCache) Connect() (string, error) {
	// get the hostname
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	hostname = strings.Split(hostname, ".")[0]
	klog.Info("hostname: ", hostname)

	out, err := cache.client.CallSBCLI("GET", "/cachingnode", nil)
	if err != nil {
		klog.Error(err)
		return "", err
	}

	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	var cnodes []*cachingNodeList
	err = json.Unmarshal(data, &cnodes)
	if err != nil {
		return "", err
	}

	klog.Info("found caching nodes: ", cnodes)

	isCachingNodeConnected := false
	for _, cnode := range cnodes {
		if hostname != cnode.Hostname {
			continue
		}

		var resp interface{}
		req := lVolCachingNodeConnect{
			LvolID: cache.lvol,
		}
		klog.Info("connecting caching node: ", cnode.Hostname, " with lvol: ", cache.lvol)
		resp, err = cache.client.CallSBCLI("PUT", "/cachingnode/connect/"+cnode.UUID, req)
		if err != nil {
			klog.Error("caching node connect error:", err)
			return "", err
		}
		klog.Info("caching node connect resp: ", resp)
		isCachingNodeConnected = true
	}

	if !isCachingNodeConnected {
		return "", errors.New("failed to find the caching node")
	}

	// get the caching node ID associated with the hostname
	// connect lvol and caching node

	deviceGlob := fmt.Sprintf(DevDiskByID, cache.model)
	devicePath, err := waitForDeviceReady(deviceGlob, 20)
	if err != nil {
		return "", err
	}
	return devicePath, nil
}

func (cache *initiatorCache) Disconnect() error {
	// get the hostname
	// get the caching node ID associated with the hostname
	// connect lvol and caching node

	hostname, err := os.Hostname()
	if err != nil {
		os.Exit(1)
	}
	hostname = strings.Split(hostname, ".")[0]
	klog.Info("hostname: ", hostname)

	out, err := cache.client.CallSBCLI("GET", "/cachingnode", nil)
	if err != nil {
		klog.Error(err)
		return err
	}

	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	var cnodes []*cachingNodeList
	err = json.Unmarshal(data, &cnodes)
	if err != nil {
		return err
	}
	klog.Info("found caching nodes: ", cnodes)

	isCachingNodeConnected := false
	for _, cnode := range cnodes {
		if hostname != cnode.Hostname {
			continue
		}
		klog.Info("disconnect caching node: ", cnode.Hostname, "with lvol: ", cache.lvol)
		req := lVolCachingNodeConnect{
			LvolID: cache.lvol,
		}
		resp, err := cache.client.CallSBCLI("PUT", "/cachingnode/disconnect/"+cnode.UUID, req)
		if err != nil {
			klog.Error("caching node disconnect error:", err)
			return err
		}
		klog.Info("caching node disconnect resp: ", resp)
		isCachingNodeConnected = true
	}

	if !isCachingNodeConnected {
		return errors.New("failed to find the caching node")
	}

	deviceGlob := fmt.Sprintf(DevDiskByID, cache.model)
	return waitForDeviceGone(deviceGlob)
}

func execWithTimeoutRetry(cmdLine []string, timeout, retry int) (err error) {
	for retry > 0 {
		err = execWithTimeout(cmdLine, timeout)
		if err == nil {
			return nil
		}
		retry--
	}
	return err
}

func (nvmf *initiatorNVMf) Connect() (string, error) {
	klog.Info("connections", nvmf.connections)
	ctrlLossTmo := 60
	if len(nvmf.connections) == 1 {
		ctrlLossTmo *= 15
	}

	alreadyConnected, err := isNqnConnected(nvmf.nqn)
	if err != nil {
		klog.Errorf("Failed to check existing connections: %v", err)
		return "", err
	}

	if !alreadyConnected {
		for i, conn := range nvmf.connections {
			cmdLine := []string{
				"nvme", "connect", "-t", strings.ToLower(nvmf.targetType),
				"-a", conn.IP, "-s", strconv.Itoa(conn.Port), "-n", nvmf.nqn, "-l", strconv.Itoa(ctrlLossTmo),
				"-c", nvmf.reconnectDelay, "-i", nvmf.nrIoQueues,
			}
			err := execWithTimeoutRetry(cmdLine, 40, len(nvmf.connections))
			if err != nil {
				// go on checking device status in case caused by duplicated request
				klog.Errorf("command %v failed: %s", cmdLine, err)

				// disconnect the primary connection if secondary connection fails
				if i == 1 {
					klog.Warning("Secondary connection failed, disconnecting primary...")

					deviceGlob := fmt.Sprintf(DevDiskByID, fmt.Sprintf("%s*_%s", nvmf.model, nvmf.nsId))
					devicePath, err := waitForDeviceReady(deviceGlob, 20)
					if err != nil {
						return "", err
					}
					err = disconnectDevicePath(devicePath)
					if err != nil {
						klog.Errorf("Failed to disconnect primary: %v", err)
						return "", err
					} else {
						klog.Infof("Primary connection disconnected due to secondary failure")
					}
				}

				return "", err
			}
		}
	}

	deviceGlob := fmt.Sprintf(DevDiskByID, fmt.Sprintf("%s*_%s", nvmf.model, nvmf.nsId))
	devicePath, err := waitForDeviceReady(deviceGlob, 20)
	if err != nil {
		return "", err
	}
	return devicePath, nil
}

func (nvmf *initiatorNVMf) Disconnect() error {
	//deviceGlob := fmt.Sprintf(DevDiskByID, nvmf.model)
	deviceGlob := fmt.Sprintf(DevDiskByID, fmt.Sprintf("%s*_[0-9]*", nvmf.model))
	devicePath, err := filepath.Glob(deviceGlob)
	if err != nil {
		return fmt.Errorf("failed to find device paths matching %s: %v", deviceGlob, err)
	}

	if len(devicePath) > 1 {
		return nil

	} else if len(devicePath) == 1 {
		err = disconnectDevicePath(devicePath[0])

		if err != nil {
			return err
		}
	}

	return waitForDeviceGone(deviceGlob)
}

// when timeout is set as 0, try to find the device file immediately
// otherwise, wait for device file comes up or timeout
func waitForDeviceReady(deviceGlob string, seconds int) (string, error) {
	for i := 0; i <= seconds; i++ {
		matches, err := filepath.Glob(deviceGlob)
		if err != nil {
			return "", err
		}
		// two symbol links under /dev/disk/by-id/ to same device
		if len(matches) >= 1 {
			return matches[0], nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("timed out waiting device ready: %s", deviceGlob)
}

// wait for device file gone or timeout
func waitForDeviceGone(deviceGlob string) error {
	for i := 0; i <= 20; i++ {
		matches, err := filepath.Glob(deviceGlob)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting device gone: %s", deviceGlob)
}

// exec shell command with timeout(in seconds)
func execWithTimeout(cmdLine []string, timeout int) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	klog.Infof("running command: %v", cmdLine)
	//nolint:gosec // execWithTimeout assumes valid cmd arguments
	cmd := exec.CommandContext(ctx, cmdLine[0], cmdLine[1:]...)
	output, err := cmd.CombinedOutput()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("timed out")
	}
	if output != nil {
		klog.Infof("command returned: %s", output)
	}
	return err
}

func disconnectDevicePath(devicePath string) error {
	var paths []path

	realPath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return fmt.Errorf("Failed to resolve device path from %s: %v", devicePath, err)
	}

	subsystems, err := getSubsystemsForDevice(realPath)
	if err != nil {
		return fmt.Errorf("Failed to get subsystems for %s: %v", realPath, err)
	}

	for _, host := range subsystems {
		for _, subsystem := range host.Subsystems {
			for _, p := range subsystem.Paths {
				paths = append(paths, path{
					Name:     p.Name,
					ANAState: p.ANAState,
				})
			}
		}
	}

	sort.Slice(paths, func(i, j int) bool {
		if paths[i].ANAState == "optimized" && paths[j].ANAState != "optimized" {
			return false
		}
		return true
	})

	for _, p := range paths {
		klog.Infof("Disconnecting device %s", p.Name)
		disconnectCmd := []string{"nvme", "disconnect", "-d", p.Name}
		err := execWithTimeoutRetry(disconnectCmd, 40, 1)
		if err != nil {
			klog.Errorf("Failed to disconnect device %s: %v", p.Name, err)
		}
	}

	return nil
}

func getNVMeDeviceInfos() ([]nvmeDeviceInfo, error) {
	cmd := exec.Command("nvme", "list", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to execute nvme list: %v", err)
	}

	var response struct {
		Devices []struct {
			DevicePath   string `json:"DevicePath"`
			SerialNumber string `json:"SerialNumber"`
		} `json:"Devices"`
	}

	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("failed to unmarshal nvme list output: %v", err)
	}

	var devices []nvmeDeviceInfo
	for _, dev := range response.Devices {
		devices = append(devices, nvmeDeviceInfo{
			devicePath:   dev.DevicePath,
			serialNumber: dev.SerialNumber,
		})
	}

	return devices, nil
}

func isNqnConnected(nqn string) (bool, error) {
	cmd := exec.Command("nvme", "list-subsys")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to execute nvme list-subsys: %v", err)
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, nqn) {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

func getSubsystemsForDevice(devicePath string) ([]subsystemResponse, error) {
	cmd := exec.Command("nvme", "list-subsys", "-o", "json", devicePath)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to execute nvme list-subsys: %v", err)
	}

	var subsystems []subsystemResponse
	if err := json.Unmarshal(output, &subsystems); err != nil {
		return nil, fmt.Errorf("failed to unmarshal nvme list-subsys output: %v", err)
	}

	return subsystems, nil
}

func getLvolIDFromNQN(nqn string) (clusterID, lvolID string) {
	parts := strings.Split(nqn, ":lvol:")
	if len(parts) > 1 {
		subparts := strings.Split(parts[0], ":")
		clusterID := subparts[len(subparts)-1]
		lvolID := parts[1]
		return clusterID, lvolID
	}
	return "", ""
}

func parseAddress(address string) string {
	parts := strings.Split(address, ",")
	for _, part := range parts {
		if strings.HasPrefix(part, "traddr=") {
			return strings.TrimPrefix(part, "traddr=")
		}
	}
	return ""
}

func reconnectSubsystems() error {
	devices, err := getNVMeDeviceInfos()
	if err != nil {
		return fmt.Errorf("failed to get NVMe device paths: %v", err)
	}

	for _, device := range devices {
		subsystems, err := getSubsystemsForDevice(device.devicePath)
		if err != nil {
			klog.Errorf("failed to get subsystems for device %s: %v", device.devicePath, err)
			continue
		}

		for _, host := range subsystems {
			for _, subsystem := range host.Subsystems {
				clusterID, lvolID := getLvolIDFromNQN(subsystem.NQN)
				if lvolID == "" {
					continue
				}

				if len(subsystem.Paths) == 1 {
					confirm := confirmSubsystemStillSinglePath(&subsystem, device.devicePath)

					if !confirm {
						continue
					}
					for _, path := range subsystem.Paths {
						if path.State == "connecting" && device.serialNumber == "single" ||
							((path.ANAState == "optimized" || path.ANAState == "non-optimized") && device.serialNumber == "ha") {
							if err := checkOnlineNode(clusterID, lvolID, path); err != nil {
								klog.Errorf("failed to reconnect subsystem for lvolID %s: %v", lvolID, err)
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func checkOnlineNode(clusterID, lvolID string, path path) error {
	sbcClient, err := NewsimplyBlockClient(clusterID)
	if err != nil {
		return fmt.Errorf("failed to create SPDK client: %w", err)
	}

	nodeInfo, err := fetchNodeInfo(sbcClient, lvolID)
	if err != nil {
		return fmt.Errorf("failed to fetch node info: %w", err)
	}

	for _, nodeID := range nodeInfo.Nodes {
		if len(nodeInfo.NodeID) > 1 && !shouldConnectToNode(path.ANAState, nodeInfo.NodeID, nodeID) {
			continue
		}

		if !isNodeOnline(sbcClient, nodeID) {
			klog.Infof("Node %s is not yet online", nodeID)
			continue
		}

		connections, err := fetchLvolConnection(sbcClient, lvolID)
		if err != nil {
			klog.Errorf("Failed to get lvol connection: %v", err)
			continue
		}

		connCount := len(connections)
		if connCount == 0 {
			klog.Warningf("No NVMe connection found for lvol %s", lvolID)
			continue
		}

		connIndex := 0
		if path.ANAState == "optimized" && connCount > 1 {
			connIndex = 1
		}
		conn := connections[connIndex]

		targetIP := parseAddress(path.Address)
		ctrlLossTmo := 60
		if connCount == 1 && conn.IP != targetIP {
			ctrlLossTmo *= 15

			if err := disconnectViaNVMe(path); err != nil {
				return err
			}
			if err := connectViaNVMe(conn, ctrlLossTmo); err != nil {
				return err
			}
			return nil
		}

		if connCount > 1 {
			if err := connectViaNVMe(conn, ctrlLossTmo); err != nil {
				return err
			}
			return nil
		}
	}

	return nil
}

func shouldConnectToNode(anaState, currentNodeID, targetNodeID string) bool {
	if anaState == "optimized" {
		return currentNodeID != targetNodeID
	}
	return currentNodeID == targetNodeID
}

func fetchNodeInfo(spdkNode *NodeNVMf, lvolID string) (*NodeInfo, error) {
	resp, err := spdkNode.Client.CallSBCLI("GET", "/lvol/"+lvolID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch node info: %v", err)
	}
	var info []NodeInfo
	respBytes, _ := json.Marshal(resp)
	if err := json.Unmarshal(respBytes, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal node info: %v", err)
	}

	if len(info) == 0 {
		return nil, fmt.Errorf("empty node info response for lvolID %s", lvolID)
	}

	return &info[0], nil
}

func isNodeOnline(spdkNode *NodeNVMf, nodeID string) bool {
	resp, err := spdkNode.Client.CallSBCLI("GET", "/storagenode/"+nodeID, nil)
	if err != nil {
		klog.Errorf("failed to fetch node status for node %s: %v", nodeID, err)
		return false
	}
	var status []NodeInfo
	respBytes, _ := json.Marshal(resp)
	if err := json.Unmarshal(respBytes, &status); err != nil {
		klog.Errorf("failed to unmarshal node status for node %s: %v", nodeID, err)
		return false
	}
	return status[0].Status == "online"
}

func fetchLvolConnection(spdkNode *NodeNVMf, lvolID string) ([]*LvolConnectResp, error) {
	resp, err := spdkNode.Client.CallSBCLI("GET", "/lvol/connect/"+lvolID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch connection: %v", err)
	}
	var connections []*LvolConnectResp
	respBytes, _ := json.Marshal(resp)
	if err := json.Unmarshal(respBytes, &connections); err != nil || len(connections) == 0 {
		return nil, fmt.Errorf("invalid or empty connection response")
	}
	return connections, nil
}

func connectViaNVMe(conn *LvolConnectResp, ctrlLossTmo int) error {
	cmd := []string{
		"nvme", "connect", "-t", "tcp",
		"-a", conn.IP, "-s", strconv.Itoa(conn.Port),
		"-n", conn.Nqn,
		"-l", strconv.Itoa(ctrlLossTmo),
		"-c", strconv.Itoa(conn.ReconnectDelay),
		"-i", strconv.Itoa(conn.NrIoQueues),
	}
	if err := execWithTimeoutRetry(cmd, 40, 1); err != nil {
		klog.Errorf("nvme connect failed: %v", err)
		return err
	}
	return nil
}

func disconnectViaNVMe(path path) error {
	cmd := []string{
		"nvme", "disconnect", "-d", path.Name,
	}
	if err := execWithTimeoutRetry(cmd, 40, 1); err != nil {
		klog.Errorf("nvme disconnect failed: %v", err)
		return err
	}
	return nil
}

func confirmSubsystemStillSinglePath(subsystem *subsystem, devicePath string) bool {
	for i := 0; i < 5; i++ {
		recheck, err := getSubsystemsForDevice(devicePath)
		if err != nil {
			klog.Errorf("failed to recheck subsystems for device %s: %v", devicePath, err)
			continue
		}

		found := false
		for _, h := range recheck {
			for _, s := range h.Subsystems {
				if s.NQN == subsystem.NQN {
					found = true
					if len(s.Paths) != 1 {
						return false
					}
				}
			}
		}

		if !found {
			klog.Warningf("Subsystem %s not found during recheck, assuming it's gone", subsystem.NQN)
			return false
		}

		time.Sleep(1 * time.Second)
	}
	return true
}

// MonitorConnection monitors the connection to the SPDK node and reconnects if necessary
// TODO: make this monitoring multiple connections
func MonitorConnection() {
	for {
		if err := reconnectSubsystems(); err != nil {
			klog.Errorf("Error: %v\n", err)
			continue
		}

		time.Sleep(3 * time.Second)
	}
}
