# Kubernetes example manifests

Reference manifests for deploying Strata on Kubernetes as a 3-replica,
stateless gateway in front of external TiKV (metadata) + RADOS (data).

| File | Purpose |
|---|---|
| `namespace.yaml` | `strata` namespace. |
| `configmap.yaml` | Non-secret env (backend selection, worker list, OTel endpoint, vhost pattern). |
| `secret.yaml` | Root S3 credentials, JWT secret, `ceph.conf` + RADOS keyring. **Replace every placeholder before applying.** |
| `deployment.yaml` | 3-replica `Deployment` with anti-affinity, `/healthz` + `/readyz` probes, ceph.conf mount, shared-JWT volume. |
| `service.yaml` | ClusterIP `Service` (Ingress target) + optional LoadBalancer. |
| `ingress.yaml` | TLS-terminated Ingress with the SigV4 Host-preservation + body-buffering-off knobs. |

## Apply order

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/configmap.yaml
kubectl apply -f deploy/k8s/secret.yaml      # edit placeholders first
kubectl apply -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/service.yaml
kubectl apply -f deploy/k8s/ingress.yaml     # if you have ingress-nginx + cert-manager
```

`kubectl -n strata rollout status deployment/strata` should report 3/3
ready within ~30 s once the image is reachable.

## Prerequisites

The gateway is stateless. Strata expects three external dependencies that
this manifest set does **not** provide:

- **TiKV** (metadata) — operate via [TiKV-Operator](https://tikv.org/docs/dev/deploy/install/install-via-tikv-operator/).
  PD ≥3, TiKV ≥3 in production. Wire `STRATA_TIKV_PD_ENDPOINTS` in
  `configmap.yaml` to the PD service.
- **RADOS** (object data) — run via [Rook-Ceph](https://rook.io/docs/rook/latest-release/Getting-Started/quickstart/).
  Mount `ceph.conf` + the RADOS keyring via the Secret.
- (Optional) **Cassandra-Operator** instead of TiKV — flip
  `STRATA_META_BACKEND=cassandra` in the ConfigMap and add Cassandra
  contact-point envs.

## Helm

Out of scope this cycle. Helm chart packaging is queued as a P3 ROADMAP
follow-up; today, apply these manifests directly.

## Apply test

The manifests have been validated with `kubectl apply --dry-run=client
-f deploy/k8s/`. Operators bringing up a kind / minikube cluster should
sub in their own image reference (`spec.template.spec.containers[0].image`)
and verify with `kubectl rollout status` end-to-end. The image
`ghcr.io/danchupin/strata:latest` in `deployment.yaml` is a placeholder —
push your own build, or build the local `strata:ceph` image via
`make docker-build` and load it into your cluster (`kind load docker-image
strata:ceph` / `minikube image load strata:ceph`) and update
`deployment.yaml` accordingly.

See [content/deploy/kubernetes.md](../../docs/site/content/deploy/kubernetes.md)
for the full operator guide.
