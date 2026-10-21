# Recipe: cert-manager (TLS certificates)

Chart `cert-manager` from https://charts.jetstack.io, release `cert-manager`, namespace `cert-manager`, values:

```yaml
crds: {enabled: true}
resources: {requests: {cpu: 10m, memory: 64Mi}}
```

The cluster is reachable only over LAN/Tailscale, so Let's Encrypt HTTP-01 does not work. Create a private CA with
apply_manifest after the chart pods are Running:

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: selfsigned}
spec: {selfSigned: {}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: homelab-ca, namespace: cert-manager}
spec:
  isCA: true
  commonName: homelab-ca
  secretName: homelab-ca
  duration: 87600h
  privateKey: {algorithm: ECDSA, size: 256}
  issuerRef: {name: selfsigned, kind: ClusterIssuer}
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: homelab-ca}
spec: {ca: {secretName: homelab-ca}}
```

Ingresses then use the annotation `cert-manager.io/cluster-issuer: homelab-ca` plus a `tls:` section. The user must
trust the CA (secret cert-manager/homelab-ca, key ca.crt) on their devices. Tailscale's own HTTPS certificates
(`tailscale cert`) are an alternative that avoids a private CA.
