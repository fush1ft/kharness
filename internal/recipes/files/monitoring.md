# Recipe: monitoring (Prometheus + Alertmanager + Grafana)

Chart `kube-prometheus-stack` from https://prometheus-community.github.io/helm-charts, release
`kube-prometheus-stack`, namespace `monitoring`. kharness expects Prometheus at
http://kube-prometheus-stack-prometheus.monitoring.svc:9090 (for the prometheus_query tool). Keep that release name.

Steps:
1. helm_upgrade_install with the values below. The chart installs its CRDs, so the first install takes a few minutes.
2. Check list_resources pods -n monitoring until everything is Running.
3. Verify with prometheus_query `up`. Alertmanager sends alerts to kharness (receiver below). The Watchdog alert is
   silenced on purpose.
4. The Grafana admin password is in secret monitoring/kube-prometheus-stack-grafana, key admin-password. Tell the
   user to read it with `kubectl -n monitoring get secret kube-prometheus-stack-grafana -o jsonpath='{.data.admin-password}' | base64 -d`.
   Never read it yourself.

k3s notes: etcd, scheduler, controller-manager and kube-proxy run inside the k3s binary and are not scraped as
separate targets, so those components are disabled (otherwise their alerts fire forever).

```yaml
kubeEtcd: {enabled: false}
kubeScheduler: {enabled: false}
kubeControllerManager: {enabled: false}
kubeProxy: {enabled: false}
defaultRules:
  rules: {etcd: false, kubeSchedulerAlerting: false, kubeSchedulerRecording: false, kubeControllerManager: false, kubeProxy: false}
prometheus:
  prometheusSpec:
    retention: 7d
    retentionSize: 8GB
    resources: {requests: {cpu: 100m, memory: 400Mi}, limits: {memory: 1500Mi}}
    serviceMonitorSelectorNilUsesHelmValues: false
    podMonitorSelectorNilUsesHelmValues: false
    ruleSelectorNilUsesHelmValues: false
    storageSpec:
      volumeClaimTemplate:
        spec: {storageClassName: local-path, accessModes: [ReadWriteOnce], resources: {requests: {storage: 10Gi}}}
alertmanager:
  alertmanagerSpec:
    resources: {requests: {cpu: 10m, memory: 32Mi}, limits: {memory: 128Mi}}
  config:
    route:
      receiver: kharness
      group_by: [alertname, namespace]
      group_wait: 30s
      group_interval: 5m
      repeat_interval: 6h
      routes:
        - {receiver: "null", matchers: ['alertname = "Watchdog"']}
        - {receiver: "null", matchers: ['alertname = "InfoInhibitor"']}
    receivers:
      - name: "null"
      - name: kharness
        webhook_configs:
          - url: http://kharness.kharness.svc:9094/alertmanager
            send_resolved: true
grafana:
  resources: {requests: {cpu: 20m, memory: 128Mi}, limits: {memory: 384Mi}}
  persistence: {enabled: true, storageClassName: local-path, size: 1Gi}
  grafana.ini:
    server: {root_url: "%(protocol)s://%(domain)s/grafana", serve_from_sub_path: true}
  ingress:
    enabled: true
    ingressClassName: traefik
    path: /grafana
    pathType: Prefix
    hosts: []
  additionalDataSources:
    - {name: Loki, type: loki, access: proxy, url: "http://loki.monitoring.svc:3100"}
```
