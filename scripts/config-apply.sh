#!/bin/bash

LABEL_KEY="type"
LABEL_VALUE="simplyblock-storage-plane"
ISOLATE_CORES="true" 
NAMESPACE="simplyblk"
NODES=$(kubectl get nodes -l "${LABEL_KEY}=${LABEL_VALUE}" -o jsonpath='{.items[*].metadata.name}')

for NODE in $NODES; do
  NODE_IP=$(kubectl get node $NODE -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
  SANITIZED_NODE=$(echo "$NODE" | tr '.' '-')
  JOB_NAME="apply-config-${SANITIZED_NODE}"

  cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB_NAME}
  namespace: ${NAMESPACE}
spec:
  template:
    spec:
      restartPolicy: Never
      nodeSelector:
        kubernetes.io/hostname: ${NODE}
      containers:
      - name: apply-config
        image: curlimages/curl:latest
        command: ["sh", "-c"]
        args:
          - |
            echo "Sending config to ${NODE_IP}";
            curl -X POST http://${NODE_IP}:5000/snode/apply_config \
              -H "Content-Type: application/json" \
              -d '{"isolate_cores": '${ISOLATE_CORES}'}'
EOF

  echo "Waiting for job ${JOB_NAME} to complete..."
  kubectl wait --for=condition=complete --timeout=300s -n ${NAMESPACE} job/${JOB_NAME}

  echo "Fetching logs from job ${JOB_NAME}..."
  POD_NAME=$(kubectl get pod -n ${NAMESPACE} --selector=job-name=${JOB_NAME} -o jsonpath='{.items[0].metadata.name}')
  kubectl logs -n ${NAMESPACE} ${POD_NAME}

  echo "Deleting job ${JOB_NAME}..."
  kubectl delete job ${JOB_NAME} -n ${NAMESPACE} --ignore-not-found
done
