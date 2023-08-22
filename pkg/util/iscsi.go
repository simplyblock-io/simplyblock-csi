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

	"k8s.io/klog"
)

const (
	numberPortalGroupTag    = 1
	numberInitiatorGroupTag = 1
	targetQueueDepth        = 64
	// SPDK ISCSI Iqn fixed prefix
	iqnPrefixName = "iqn.2016-06.io.spdk:"
)

type nodeISCSI struct {
	client     *rpcClient
	targetAddr string
	targetPort string
}

func newISCSI(client *rpcClient, targetAddr string) *nodeISCSI {
	return &nodeISCSI{
		client:     client,
		targetAddr: targetAddr,
		targetPort: cfgISCSISvcPort,
	}
}

func (node *nodeISCSI) Info() string {
	return node.client.info()
}

func (node *nodeISCSI) LvStores() ([]LvStore, error) {
	return node.client.lvStores()
}

// VolumeInfo returns a string:string map containing information necessary
// for CSI node(initiator) to connect to this target and identify the disk.
func (node *nodeISCSI) VolumeInfo(lvolID string) (map[string]string, error) {
	exists, err := node.isVolumeCreated(lvolID)
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, fmt.Errorf("volume not exists: %s", lvolID)
	}

	return map[string]string{
		"targetAddr": node.targetAddr,
		"targetPort": node.targetPort,
		"iqn":        iqnPrefixName + lvolID,
		"targetType": "iscsi",
	}, nil
}

// CreateVolume creates a logical volume and returns volume ID
func (node *nodeISCSI) CreateVolume(lvolName, lvsName string, sizeMiB int64) (string, error) {
	// all volume have an alias ID named lvsName/lvolName
	lvol, err := node.client.getVolume(fmt.Sprintf("%s/%s", lvsName, lvolName))
	if err == nil {
		klog.Warningf("volume already created: %s", lvol.UUID)
		return lvol.UUID, nil
	}

	lvolID, err := node.client.createVolume(lvolName, lvsName, sizeMiB)
	if err != nil {
		return "", err
	}
	klog.V(5).Infof("volume created: %s", lvolID)
	return lvolID, nil
}

// GetVolume returns the volume id of the given volume name and lvstore name. return error if not found.
func (node *nodeISCSI) GetVolume(lvolName, lvsName string) (string, error) {
	lvol, err := node.client.getVolume(fmt.Sprintf("%s/%s", lvsName, lvolName))
	if err != nil {
		return "", err
	}
	return lvol.UUID, err
}

func (node *nodeISCSI) isVolumeCreated(lvolID string) (bool, error) {
	return node.client.isVolumeCreated(lvolID)
}

func (node *nodeISCSI) CreateSnapshot(lvolName, snapshotName string) (string, error) {
	lvsName, err := node.client.getLvstore(lvolName)
	if err != nil {
		return "", err
	}
	lvol, err := node.client.getVolume(fmt.Sprintf("%s/%s", lvsName, snapshotName))
	if err == nil {
		klog.Warningf("snapshot already created: %s", lvol.UUID)
		return lvol.UUID, nil
	}
	snapshotID, err := node.client.snapshot(lvolName, snapshotName)
	if err != nil {
		return "", err
	}

	klog.V(5).Infof("snapshot created: %s", snapshotID)
	return snapshotID, nil
}

func (node *nodeISCSI) DeleteVolume(lvolID string) error {
	err := node.client.deleteVolume(lvolID)
	if err != nil {
		return err
	}

	klog.V(5).Infof("volume deleted: %s", lvolID)
	return nil
}

func (node *nodeISCSI) DeleteSnapshot(snapshotID string) error {
	err := node.client.deleteVolume(snapshotID)
	if err != nil {
		return err
	}
	klog.V(5).Infof("snapshot deleted: %s", snapshotID)
	return nil
}

// PublishVolume exports a volume through ISCSI target
func (node *nodeISCSI) PublishVolume(lvolID string) error {
	exists, err := node.isVolumeCreated(lvolID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrVolumeDeleted
	}
	published, err := node.isVolumePublished(lvolID)
	if err != nil {
		return err
	}
	if published {
		return nil
	}

	err = node.createPortalGroup()
	if err != nil {
		return err
	}

	err = node.createInitiatorGroup()
	if err != nil {
		return err
	}
	// lvolID is unique and can be used as the target name
	targetName := lvolID
	err = node.iscsiCreateTargetNode(targetName, lvolID)
	if err != nil {
		return err
	}

	return nil
}

func (node *nodeISCSI) isVolumePublished(lvolID string) (bool, error) {
	var result []struct {
		Name      string `json:"name"`
		AliasName string `json:"alias_name"`
	}
	// TODO: newer version of SPDK supports passing an alias_name parameter for filtering.
	//       better to add name parameter after the CI's SPDK is upgraded.
	err := node.client.call("iscsi_get_target_nodes", nil, &result)
	if err != nil {
		return false, err
	}
	for i := range result {
		if result[i].AliasName == lvolID {
			return true, nil
		}
	}
	return false, nil
}

func (node *nodeISCSI) createPortalGroup() error {
	err := node.iscsiGetPortalGroups()
	if err == nil {
		return nil // port group already exists
	}

	err = node.iscsiCreatePortalGroup()
	if err == nil {
		return nil // creation succeeds
	}
	// we may fail due to concurrent calls, check portal group availability again
	return node.iscsiGetPortalGroups()
}

func (node *nodeISCSI) createInitiatorGroup() error {
	err := node.iscsiGetInitiatorGroups()
	if err == nil {
		return nil
	}

	err = node.iscsiCreateInitiatorGroup([]string{"ANY"}, []string{"ANY"})
	if err == nil {
		return nil
	}

	return node.iscsiGetInitiatorGroups()
}

func (node *nodeISCSI) UnpublishVolume(lvolID string) error {
	exists, err := node.isVolumeCreated(lvolID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrVolumeDeleted
	}
	published, err := node.isVolumePublished(lvolID)
	if err != nil {
		return err
	}
	if !published {
		// already unpublished
		return nil
	}

	err = node.iscsiDeleteTargetNode(lvolID)
	if err != nil {
		return err
	}

	klog.V(5).Infof("volume unpublished: %s", lvolID)
	return nil
}

// Add a portal group
func (node *nodeISCSI) iscsiCreatePortalGroup() error {
	type Portals struct {
		Host string `json:"host"`
		Port string `json:"port"`
	}
	params := struct {
		Portals []Portals `json:"portals"`
		Tag     int       `json:"tag"`
	}{
		Portals: []Portals{{node.targetAddr, node.targetPort}},
		Tag:     numberPortalGroupTag,
	}
	var result bool
	err := node.client.call("iscsi_create_portal_group", &params, &result)
	if err != nil {
		return err
	}
	if !result {
		return fmt.Errorf("create iscsi portal group failure")
	}
	return nil
}

// Add an initiator group
func (node *nodeISCSI) iscsiCreateInitiatorGroup(initiators, netmasks []string) error {
	params := struct {
		Initiators []string `json:"initiators"`
		Tag        int      `json:"tag"`
		Netmasks   []string `json:"netmasks"`
	}{
		Initiators: initiators,
		Tag:        numberInitiatorGroupTag,
		Netmasks:   netmasks,
	}
	var result bool
	err := node.client.call("iscsi_create_initiator_group", &params, &result)
	if err != nil {
		return err
	}
	if !result {
		return fmt.Errorf("create iscsi initiator group failure")
	}
	return nil
}

// Add an iSCSI target node
func (node *nodeISCSI) iscsiCreateTargetNode(targetName, bdevName string) error {
	type Luns struct {
		LunID    int    `json:"lun_id"`
		BdevName string `json:"bdev_name"`
	}
	type PgIgMaps struct {
		IgTag int `json:"ig_tag"`
		PgTag int `json:"pg_tag"`
	}
	params := struct {
		Luns        []Luns     `json:"luns"`
		Name        string     `json:"name"`
		AliasName   string     `json:"alias_name"`
		PgIgMaps    []PgIgMaps `json:"pg_ig_maps"`
		DisableChap bool       `json:"disable_chap"`
		QueueDepth  int        `json:"queue_depth"`
	}{
		Luns: []Luns{{0, bdevName}},
		Name: targetName,
		// Set aliasName equal to targetName (which is lvolID) for convenience
		// so that the function "isVolumePublished" can check if the volume is
		// already published using aliasName(lvolID) easily.
		AliasName:   targetName,
		PgIgMaps:    []PgIgMaps{{numberPortalGroupTag, numberInitiatorGroupTag}},
		DisableChap: true,
		QueueDepth:  targetQueueDepth,
	}
	var result bool
	err := node.client.call("iscsi_create_target_node", &params, &result)
	if err != nil {
		return err
	}
	if !result {
		return fmt.Errorf("create iscsi target node failure")
	}
	return nil
}

// Delete an iSCSI target node
func (node *nodeISCSI) iscsiDeleteTargetNode(targetName string) error {
	params := struct {
		Name string `json:"name"`
	}{
		Name: iqnPrefixName + targetName,
	}
	var result bool
	err := node.client.call("iscsi_delete_target_node", &params, &result)
	if err != nil {
		return err
	}
	if !result {
		return fmt.Errorf("delete iscsi target node failure")
	}
	return nil
}

// Check if portal group is available
func (node *nodeISCSI) iscsiGetPortalGroups() error {
	var results []struct {
		Tag int `json:"tag"`
	}
	err := node.client.call("iscsi_get_portal_groups", nil, &results)
	if err != nil {
		return err
	}
	for _, value := range results {
		if value.Tag == numberPortalGroupTag {
			return nil
		}
	}
	return fmt.Errorf("port group not available")
}

func (node *nodeISCSI) iscsiGetInitiatorGroups() error {
	var results []struct {
		Tag int `json:"tag"`
	}
	err := node.client.call("iscsi_get_initiator_groups", nil, &results)
	if err != nil {
		return err
	}
	for _, value := range results {
		if value.Tag == numberInitiatorGroupTag {
			return nil
		}
	}
	return fmt.Errorf("initiator group not available")
}
