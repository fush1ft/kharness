# Recipe: app (deploy an application)

Template for a stateless web app, applied with apply_manifest. Adjust the name, image, port and host. Rules:
- One namespace per app (or a shared `apps` namespace). Never deploy to `default` or `kube-system`.
- Always set resource requests/limits and a readinessProbe. The node is small and shared.
- Images built by the user are pushed to the node-local registry as `localhost:5000/<name>:<tag>`.
- Ingress goes through the bundled Traefik (ingressClassName traefik). Use a path prefix on the node's
  Tailscale host, or a dedicated host name.
- Persistent data: a PVC with storageClassName local-path. Use Recreate for single-replica apps with an RWO volume.

```yaml
apiVersion: v1
kind: Namespace
metadata: {name: myapp}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: myapp, namespace: myapp, labels: {app: myapp}}
spec:
  replicas: 1
  selector: {matchLabels: {app: myapp}}
  template:
    metadata: {labels: {app: myapp}}
    spec:
      containers:
        - name: myapp
          image: nginxinc/nginx-unprivileged:stable-alpine
          ports: [{containerPort: 8080, name: http}]
          readinessProbe: {httpGet: {path: /, port: http}, periodSeconds: 10}
          resources: {requests: {cpu: 10m, memory: 32Mi}, limits: {memory: 128Mi}}
---
apiVersion: v1
kind: Service
metadata: {name: myapp, namespace: myapp}
spec:
  selector: {app: myapp}
  ports: [{port: 80, targetPort: http, name: http}]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: myapp, namespace: myapp}
spec:
  ingressClassName: traefik
  rules:
    - http:
        paths:
          - {path: /myapp, pathType: Prefix, backend: {service: {name: myapp, port: {name: http}}}}
```

If the app does not understand the path prefix, add a Traefik StripPrefix middleware:
`traefik.io/v1alpha1` Middleware `{spec: {stripPrefix: {prefixes: [/myapp]}}}`, then annotate the Ingress with
`traefik.ingress.kubernetes.io/router.middlewares: myapp-strip@kubernetescrd` (format `<namespace>-<name>@kubernetescrd`).

After applying, watch the rollout: list_resources pods -n myapp, then list_events for anything not Ready.
