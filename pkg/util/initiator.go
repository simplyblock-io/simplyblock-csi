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
	"strconv"
	"strings"
	"time"

	"k8s.io/klog"
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

const DevDiskByID = "/dev/disk/by-id/*%s*"

func NewSpdkCsiInitiator(volumeContext map[string]string, spdkNode *NodeNVMf) (SpdkCsiInitiator, error) {
	targetType := strings.ToLower(volumeContext["targetType"])
	switch targetType {
	case "rdma", "tcp":
		var connections []connectionInfo
		err := json.Unmarshal([]byte(volumeContext["connections"]), &connections)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshall connections. Error: %v", err.Error())
		}
		return &initiatorNVMf{
			// see util/nvmf.go VolumeInfo()
			targetType:  volumeContext["targetType"],
			connections: connections,
			nqn:         volumeContext["nqn"],
			model:       volumeContext["model"],
		}, nil
	case "cache":
		return &initiatorCache{
			lvol:   volumeContext["uuid"],
			model:  volumeContext["model"],
			client: *spdkNode.client,
		}, nil
	default:
		return nil, fmt.Errorf("unknown initiator: %s", targetType)
	}
}

// NVMf initiator implementation
type initiatorNVMf struct {
	targetType  string
	connections []connectionInfo
	nqn         string
	model       string
}

type initiatorCache struct {
	lvol   string
	model  string
	client RPCClient
}

type cachingNodeList struct {
	Hostname string `json:"hostname"`
	UUID     string `json:"id"`
}

type LVolCachingNodeConnect struct {
	LvolID string `json:"lvol_id"`
}

func (cache *initiatorCache) Connect() (string, error) {
	// get the hostname
	hostname, err := os.Hostname()
	if err != nil {
		os.Exit(1)
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
		req := LVolCachingNodeConnect{
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
		req := LVolCachingNodeConnect{
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
	// nvme connect -t tcp -a 192.168.1.100 -s 4420 -n "nqn"
	klog.Info("connections", nvmf.connections)
	for _, conn := range nvmf.connections {
		cmdLine := []string{
			"nvme", "connect", "-t", strings.ToLower(nvmf.targetType),
			"-a", conn.IP, "-s", strconv.Itoa(conn.Port), "-n", nvmf.nqn, "-l", "30",
		}
		err := execWithTimeoutRetry(cmdLine, 40, len(nvmf.connections))
		if err != nil {
			// go on checking device status in case caused by duplicated request
			klog.Errorf("command %v failed: %s", cmdLine, err)
			return "", err
		}
	}

	deviceGlob := fmt.Sprintf(DevDiskByID, nvmf.model)
	devicePath, err := waitForDeviceReady(deviceGlob, 20)
	if err != nil {
		return "", err
	}
	return devicePath, nil
}

func (nvmf *initiatorNVMf) Disconnect() error {
	// nvme disconnect -n "nqn"
	cmdLine := []string{"nvme", "disconnect", "-n", nvmf.nqn}
	err := execWithTimeout(cmdLine, 40)
	if err != nil {
		// go on checking device status in case caused by duplicate request
		klog.Errorf("command %v failed: %s", cmdLine, err)
	}

	deviceGlob := fmt.Sprintf(DevDiskByID, nvmf.model)
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
