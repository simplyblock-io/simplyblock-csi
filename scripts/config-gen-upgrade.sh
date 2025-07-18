#!/bin/bash

LABEL_KEY="type"
LABEL_VALUE="simplyblock-storage-plane"
IMAGE_REPO="simplyblock/simplyblock"
IMAGE_TAG=<VALUE>
IMAGE_PULL_POLICY="Always"
NAMESPACE="${1:-simplyblock}" 

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
            - "--upgrade"
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
