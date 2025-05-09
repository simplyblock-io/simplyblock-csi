#!/bin/bash

LABEL_KEY="type"
LABEL_VALUE="simplyblock-storage-plane"
IMAGE_REPO="simplyblock/simplyblock"
IMAGE_TAG="main"
IMAGE_PULL_POLICY="Always"
NAMESPACE="simplyblk"
MAX_LVOL=<VALUE>
MAX_PROV=<VALUE>
PCI_ALLOWED="<COMMA_SAPARATED_LIST>"
PCI_BLOCKED=""
SOCKETS_TO_USE=""
NODES_PER_SOCKET=""

NODES=$(kubectl get nodes -l "${LABEL_KEY}=${LABEL_VALUE}" -o jsonpath='{.items[*].metadata.name}')

for NODE in $NODES; do
  SANITIZED_NODE=$(echo "$NODE" | tr '.' '-')
  JOB_NAME="simplyblock-upgrade-${SANITIZED_NODE}"
  
  cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB_NAME}
  namespace: ${NAMESPACE}
spec:
  template:
    spec:
      restartPolicy: OnFailure
      nodeSelector:
        kubernetes.io/hostname: ${NODE}
      hostNetwork: true
      serviceAccountName: simplyblock-storage-node-sa
      volumes:
        - name: etc-simplyblock
          hostPath:
            path: /var/simplyblock
      containers:
        - name: s-node-api-config-generator
          image: ${IMAGE_REPO}:${IMAGE_TAG}
          imagePullPolicy: ${IMAGE_PULL_POLICY}
          command:
            - "python"
            - "simplyblock_web/node_configure.py"
            - "--max-lvol=${MAX_LVOL}"
            - "--max-size=${MAX_PROV}"
            - "--upgrade"
            $( [ -n "${PCI_ALLOWED}" ] && echo "- --pci-allowed=${PCI_ALLOWED}" )
            $( [ -n "${PCI_BLOCKED}" ] && echo "- --pci-blocked=${PCI_BLOCKED}" )
            $( [ -n "${SOCKETS_TO_USE}" ] && echo "- --sockets-to-use=${SOCKETS_TO_USE}" )
            $( [ -n "${NODES_PER_SOCKET}" ] && echo "- --nodes-per-socket=${NODES_PER_SOCKET}" )
          volumeMounts:
            - name: etc-simplyblock
              mountPath: /etc/simplyblock
          securityContext:
            privileged: true
EOF

  echo "Waiting for job ${JOB_NAME} to complete..."
  kubectl wait --for=condition=complete --timeout=300s -n ${NAMESPACE} job/${JOB_NAME}

  echo "Fetching logs from job ${JOB_NAME}..."
  POD_NAME=$(kubectl get pod -n ${NAMESPACE} --selector=job-name=${JOB_NAME} -o jsonpath='{.items[0].metadata.name}')
  kubectl logs -n ${NAMESPACE} ${POD_NAME}
  
  echo "Deleting job ${JOB_NAME}..."
  kubectl delete job ${JOB_NAME} -n ${NAMESPACE} --ignore-not-found

done
