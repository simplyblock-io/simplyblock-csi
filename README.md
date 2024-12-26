# Simplyblock CSI Driver for Kubernetes

This repo contains Simplyblock CSI ([Container Storage Interface](https://github.com/container-storage-interface/))
plugin for Kubernetes.

Simplyblock CSI plugin brings high performance block storage to Kubernetes. It provisions SPDK logical volumes on storage node dynamically and enables Pods to access SPDK storage backend through NVMe-oF .

Most parts of the CSI driver are vey much similar to the original [SPDK CSI Design Document](https://docs.google.com/document/d/1aLi6SkNBp__wjG7YkrZu7DdhoftAquZiWiIOMy3hskY/)


### Project status: Beta

### Supported Features
- Dynamic Volume Provisioning
- Dynamic Volume Provisioning for Caching nodes
- Volume Snapshots

### Container Images & Kubernetes Compatibility:
| driver version | supported k8s version | status |
| -------------- | --------------------- | ------ |
| master branch  | 1.21+                 | Beta   |
| v0.1.0         | 1.21+                 | Beta   |
| v0.1.1         | 1.21+                 | Beta   |

### Install driver on a Kubernetes cluster
 - install via [helm charts](./charts)
 - install via [kubectl](./docs/install-simplyblock-csi-driver.md)

### Driver parameters
Please refer to [`csi.simplyblock.io` driver parameters](./charts/README.md#driver-parameters)

### Troubleshooting
 - [CSI driver troubleshooting guide](./docs/csi-debug.md)

### Supported Worker node types

The CSI driver is currently tested with various types of worker nodes.

On AWS EKS the following worker nodes types are supported:
* AmazonLinux2 (AL_2_x86_64)
* AmazonLinux2023 (AL_2023_x86_64_STANDARD)

On K3S the following Worker nodes are supported:
* RHEL9
* Ubuntu 22.04
* AmazonLinux2
* AmazonLinux2023
