# Recipe: loki (central logs)

Two releases in namespace `monitoring`. Install the monitoring recipe first so Grafana picks Loki up as a data
source. kharness expects Loki at http://loki.monitoring.svc:3100 (for the loki_query tool).

1. Chart `loki` from https://grafana.github.io/helm-charts, release `loki` (single binary, filesystem storage, 7 days):

```yaml
deploymentMode: SingleBinary
loki:
  auth_enabled: false
  commonConfig: {replication_factor: 1}
  storage: {type: filesystem}
  schemaConfig:
    configs:
      - {from: "2024-04-01", store: tsdb, object_store: filesystem, schema: v13, index: {prefix: loki_index_, period: 24h}}
  limits_config: {retention_period: 168h}
  compactor: {retention_enabled: true, delete_request_store: filesystem}
singleBinary:
  replicas: 1
  persistence: {enabled: true, size: 10Gi, storageClass: local-path}
  resources: {requests: {cpu: 50m, memory: 256Mi}, limits: {memory: 1Gi}}
read: {replicas: 0}
write: {replicas: 0}
backend: {replicas: 0}
gateway: {enabled: false}
chunksCache: {enabled: false}
resultsCache: {enabled: false}
lokiCanary: {enabled: false}
test: {enabled: false}
```

2. Chart `alloy` from https://grafana.github.io/helm-charts, release `alloy`. It tails pod logs through the API
   server and ships Kubernetes events as well:

```yaml
alloy:
  resources: {requests: {cpu: 20m, memory: 64Mi}, limits: {memory: 256Mi}}
  configMap:
    content: |
      discovery.kubernetes "pods" {
        role = "pod"
      }
      discovery.relabel "pods" {
        targets = discovery.kubernetes.pods.targets
        rule {
          source_labels = ["__meta_kubernetes_namespace"]
          target_label  = "namespace"
        }
        rule {
          source_labels = ["__meta_kubernetes_pod_name"]
          target_label  = "pod"
        }
        rule {
          source_labels = ["__meta_kubernetes_pod_container_name"]
          target_label  = "container"
        }
      }
      loki.source.kubernetes "pods" {
        targets    = discovery.relabel.pods.output
        forward_to = [loki.write.default.receiver]
      }
      loki.source.kubernetes_events "events" {
        forward_to = [loki.write.default.receiver]
      }
      loki.write "default" {
        endpoint {
          url = "http://loki.monitoring.svc:3100/loki/api/v1/push"
        }
      }
```

3. Verify after a minute with loki_query `{namespace="kube-system"}`.
