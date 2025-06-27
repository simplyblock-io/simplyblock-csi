### Multi Cluster Support

The Simplyblock CSI driver now offers **multi-cluster support**, allowing it to connect with multiple Simplyblock clusters. Previously, the CSI driver could only connect to a single cluster.

To enable interaction with multiple clusters, we've introduced two key changes:

1.  **`cluster_id` Parameter in Storage Class:** A new parameter, `cluster_id`, has been added to the storage class. This parameter specifies which Simplyblock cluster a given request should be directed to.
2.  **`simplyblock-csi-secret-v2` Secret:** A new Kubernetes secret, `simplyblock-csi-secret-v2`, is now used to store credentials for all configured Simplyblock clusters.


#### Adding new cluster

When the Simplyblock CSI driver is initially installed, typically using Helm:
```
helm install simplyblock-csi ./ \
    --set csiConfig.simplybk.uuid=${CLUSTER_ID} \
    --set csiConfig.simplybk.ip=${CLUSTER_IP} \
    --set csiSecret.simplybk.secret=${CLUSTER_SECRET} \
```

The `CLUSTER_ID` (UUID), Gateway Endpoint (`CLUSTER_IP`), and Secret (`CLUSTER_SECRET`) of the initial cluster must be provided. This command automatically creates the `simplyblock-csi-secret-v2` secret.

The structure of the simplyblock-csi-secret-v2 secret looks like this:

```yaml
apiVersion: v1
data:
  secret.json: <base64 encoded secret>
kind: Secret
metadata:
  name: simplyblock-csi-secret-v2
type: Opaque
```

and the decoded secret looks something like this
```
{
   "clusters": [
     {
       "cluster_id": "4ec308a1-61cf-4ec6-bff9-aa837f7bc0ea",
       "cluster_endpoint": "http://127.0.0.1",
       "cluster_secret": "super_secret"
     }
   ]
}
```

To add a new cluster, we will need to edit this secret and add a new cluster


```sh
# save cluster secret to a file
kubectl get secret simplyblock-csi-secret-v2 -o jsonpath='{.data.secret\.json}' | base64 --decode > secret.yaml

# edit the clusters and add the new cluster's cluster_id, cluster_endpoint, cluster_secret
# vi secret.json 

cat secret.json | base64 | tr -d '\n' > secret-encoded.json

# Replace data.secret.json with the content of secret-encoded.json
# kubectl -n simplyblock edit secret simplyblock-csi-secret-v2
```


```yaml
apiVersion: v1
data:
  secret.json: <new content of the secret-encoded.json>
kind: Secret
metadata:
  name: simplyblock-csi-secret-v2
type: Opaque
```

### using multi cluster

With multi-cluster support enabled, it's highly recommended to create a separate storage class for each Simplyblock cluster. This provides clear segregation and management.

For example:

* `simplyblock-csi-sc-cluster1` (for `cluster_id: 4ec308a1-...`)

* `simplyblock-csi-sc-cluster2` (for `cluster_id: YOUR_NEW_CLUSTER_ID`)

Each storage class would then specify its corresponding cluster_id in its parameters.
