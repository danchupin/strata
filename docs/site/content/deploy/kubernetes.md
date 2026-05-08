---
title: 'Kubernetes'
weight: 40
description: 'Deploy Strata on Kubernetes as a 3-replica stateless gateway in front of external TiKV + RADOS.'
---

# Kubernetes deployment

Strata on Kubernetes is a stateless gateway tier in front of external
metadata (TiKV / Cassandra) and object-data (RADOS / S3) clusters. Pods
are interchangeable — no PVCs, no per-pod storage, no quorum among
gateways. Scale horizontally with `kubectl scale`; durability lives in
the storage tier.

This page walks through the worked example committed under
[`deploy/k8s/`](https://github.com/danchupin/strata/tree/main/deploy/k8s)
— a 3-replica `Deployment` + `Service` + `ConfigMap` + `Secret` +
`Ingress` aimed at a production-shaped cluster.

## Architecture

```
                +-------------------+
   client ---> |  Ingress (nginx)  |  TLS terminate, Host preserved,
                +---------+---------+  request buffering off
                          |
                          v
                +-------------------+
                |  Service (Cluster |
                |  IP, port 9000)   |
                +---------+---------+
                          |
            +-------------+-------------+
            |             |             |
        +---v---+     +---v---+     +---v---+
        | strata|     | strata|     | strata|
        | pod 0 |     | pod 1 |     | pod 2 |  Deployment, replicas=3
        +-------+     +-------+     +-------+  podAntiAffinity per node
            |             |             |
            +------+------+------+------+
                   |             |
                   v             v
             +-----------+  +----------+
             |   TiKV    |  |  RADOS   |   external clusters,
             | (PD ≥3,   |  | (Rook,   |   not in this manifest set
             |  TiKV ≥3) |  | size=3)  |
             +-----------+  +----------+
```

The gateway is **stateless**. Replica failure has no effect on data;
the LB drains the dead pod, surviving replicas pick up worker leases
within ~30 s.

## Prerequisites

- A Kubernetes cluster ≥1.27 with **ingress-nginx** + **cert-manager**
  (or your preferred ingress + TLS chain).
- An external **TiKV** cluster — install via
  [TiKV-Operator](https://tikv.org/docs/dev/deploy/install/install-via-tikv-operator/).
  PD ≥3 + TiKV ≥3 in production.
- An external **RADOS** pool — install via
  [Rook-Ceph](https://rook.io/docs/rook/latest-release/Getting-Started/quickstart/)
  (`size=3` replication factor).
- A container image. The example points at
  `ghcr.io/danchupin/strata:latest` — push your own build, or load the
  locally-built `strata:ceph` (`make docker-build`) into a kind /
  minikube cluster.

PD/TiKV stateful sets are out of scope for this guide — operators run
TiKV-Operator and treat its endpoints as a dependency. Same goes for
Cassandra-Operator if you flip the metadata backend.

## What ships under `deploy/k8s/`

| File | Purpose |
|---|---|
| `namespace.yaml` | `strata` namespace + labels. |
| `configmap.yaml` | Non-secret env: backend selection, worker list, OTel, vhost pattern, GC shard count. |
| `secret.yaml` | S3 root credentials, JWT secret, `ceph.conf`, RADOS keyring. Placeholders to replace before apply. |
| `deployment.yaml` | 3-replica gateway, podAntiAffinity, `/readyz` + `/healthz` probes, securityContext, JWT-shared volume. |
| `service.yaml` | ClusterIP (Ingress target) + optional LoadBalancer. |
| `ingress.yaml` | TLS-terminated `Ingress` with the SigV4 Host-preservation + body-buffering-off knobs. |

Apply order:

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/configmap.yaml
kubectl apply -f deploy/k8s/secret.yaml      # replace placeholders first
kubectl apply -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/service.yaml
kubectl apply -f deploy/k8s/ingress.yaml     # if you have ingress-nginx + cert-manager
kubectl -n strata rollout status deployment/strata
```

The rollout reports 3/3 ready within ~30 s once the image is reachable
and TiKV/RADOS are up.

## Configuration via ConfigMap + Secret

The `Deployment` pulls non-secret env from `strata-config` (ConfigMap)
and credentials from `strata-secrets` (Secret) via `envFrom:`. The split
keeps the Deployment manifest clean and lets you rotate secrets without
touching the Deployment object.

Knobs of note in the ConfigMap:

| Env | Why |
|---|---|
| `STRATA_META_BACKEND=tikv` | The example points at TiKV. Flip to `cassandra` (and add Cassandra envs) if you run Cassandra-Operator instead. |
| `STRATA_TIKV_PD_ENDPOINTS` | PD service address (cluster-DNS form). Multi-PD: comma-separate. |
| `STRATA_DATA_BACKEND=rados` | RADOS for object data. The pod image must be the `ceph`-tagged build (`make docker-build`). |
| `STRATA_WORKERS` | Which workers the binary owns. Default in the example: `gc,lifecycle,notify,replicator,access-log,inventory,audit-export`. Trim to the subset you need. |
| `STRATA_GC_SHARDS=3` | Equal to the replica count. Each replica owns one logical GC shard; survivors pick up dead replicas' shards within ~30 s. |
| `STRATA_AUTH_MODE=required` | Production. Never run `optional` outside the lab profile. |
| `STRATA_VHOST_PATTERN` | Set to match the wildcard host in your Ingress (e.g. `*.s3.example.com`). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTel collector. Tail-sampler exports failing spans regardless of `STRATA_OTEL_SAMPLE_RATIO`. |
| `STRATA_OTEL_RINGBUF*` | In-process trace browser fed via the admin console. |

Knobs of note in the Secret:

| Field | Why |
|---|---|
| `STRATA_STATIC_CREDENTIALS` | `<access>:<secret>:<owner>[,<access>:<secret>:<owner>]`. Owner string defaults to `owner` for the IAM root principal. |
| `STRATA_CONSOLE_JWT_SECRET` | 32-byte hex (`openssl rand -hex 32`). Set this so console sessions survive every replica restart and pod IP change. Without it the gateway falls back to file-based bootstrap on `/etc/strata/jwt-shared/secret` (works, but tied to the shared volume's lifecycle). |
| `ceph.conf` + `keyring` | Mounted at `/etc/ceph` via the Secret's projected items. RADOS user matches `STRATA_RADOS_USER`. |

In production, never commit a rendered `secret.yaml`. Manage these via
[sealed-secrets](https://github.com/bitnami-labs/sealed-secrets),
[external-secrets](https://external-secrets.io/), or
[vault-csi-provider](https://github.com/hashicorp/vault-csi-provider).

## Health probes

The Deployment wires:

- **Readiness**: HTTP `GET /readyz` — the LB drains a pod whose
  metadata or RADOS backend is degraded. Strata's `/readyz` runs both
  probes concurrently with a 1 s timeout.
- **Liveness**: HTTP `GET /healthz` — always 200 unless the process is
  unresponsive. Failure threshold is loose (60 s) so transient stalls
  don't restart pods needlessly.

Keep the **readiness probe on `/readyz`, not `/healthz`**. The latter
returns 200 unconditionally; an Ingress that scopes against it will
keep sending traffic to a pod with a sick metadata cluster.

## Anti-affinity + spread

`podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution`
spreads replicas across nodes so a single-node loss takes down at
most one replica. `preferred` (vs `required`) lets the scheduler pack
when the cluster is small (e.g. kind / minikube — 1 node), without
blocking the rollout.

For multi-AZ deployments, add a second `podAntiAffinity` term keyed on
`topology.kubernetes.io/zone`, or layer
`topologySpreadConstraints` for finer control.

## SecurityContext

Container runs as non-root (`runAsUser: 65532`), with
`readOnlyRootFilesystem: true`, all Linux caps dropped, and
`allowPrivilegeEscalation: false`. The `ceph.conf` and JWT-shared
volumes are the only writable paths the binary needs — neither is on
the rootfs.

Pin the seccomp profile and PodSecurityStandards label your namespace
to `restricted` if your cluster enforces them; the example complies
out of the box.

## Ingress + the SigV4 Host gotcha

SigV4 signs the `Host` header. Any LB / proxy that rewrites it breaks
**every** signed request. The reference Ingress sets:

- `nginx.ingress.kubernetes.io/upstream-vhost: "$host"` — preserves
  the original Host header on the upstream connection.
- `nginx.ingress.kubernetes.io/proxy-request-buffering: "off"` +
  `proxy-buffering: "off"` + `proxy-body-size: "0"` — streams
  request bodies through, required for SigV4 chunked uploads + large
  multipart parts.
- `proxy-read-timeout` / `proxy-send-timeout` raised to 300 s so a
  slow client doesn't get cut off mid-multipart.

Two host rules are configured: the bare `s3.example.com` (path-style
URLs) and a wildcard `*.s3.example.com` (virtual-hosted-style URLs).
The wildcard host needs a wildcard TLS cert (cert-manager
`Certificate` with `dnsNames: [s3.example.com, "*.s3.example.com"]`)
and a DNS provider that can prove ACME DNS-01 ownership.

`STRATA_VHOST_PATTERN` in the ConfigMap **must** include the wildcard
suffix you serve (e.g. `*.s3.example.com`), otherwise vhost
URLs route as path-style with the bucket-name in the leftmost host
label and the gateway returns NoSuchBucket.

## Storage

The gateway pod has no persistent storage of its own. The two volumes
the Deployment mounts are:

| Mount | Source | Purpose |
|---|---|---|
| `/etc/ceph` | `Secret` projected items (`ceph.conf`, `keyring`) | RADOS client config. |
| `/etc/strata/jwt-shared` | `emptyDir` (medium=Memory) | First-boot JWT-secret bootstrap (`internal/serverapp.loadJWTSecret`). Skip if `STRATA_CONSOLE_JWT_SECRET` is set in the Secret — env wins, the file is never written. |

Cassandra / TiKV / RADOS PVCs are owned by their respective operators —
not this manifest set. The Strata Deployment never claims a PVC.

## JWT secret distribution

If `STRATA_CONSOLE_JWT_SECRET` is set in `strata-secrets` (recommended
for production), every replica reads it from env at boot and console
sessions survive any LB-flip across replicas.

If unset, every pod tries to bootstrap a shared file at
`/etc/strata/jwt-shared/secret` via POSIX `O_EXCL`. That works against
a `ReadWriteMany` PVC mounted on every replica, but **does not work
against the `emptyDir` volume in the example manifest** — `emptyDir`
is per-pod, so each replica generates its own secret and console
sessions break across LB-flips. The example sets the env in the
Secret precisely to avoid this trap; if you remove it, swap the
`emptyDir` for a `ReadWriteMany` PVC backed by NFS / Rook-CephFS /
your provider's RWX class.

## Scaling

```bash
kubectl -n strata scale deployment/strata --replicas=5
```

Then bump `STRATA_GC_SHARDS` in the ConfigMap to match the new replica
count and `kubectl rollout restart deployment/strata` so each replica
re-reads the value. STRATA_GC_SHARDS less than the replica count
starves some replicas of GC work; STRATA_GC_SHARDS greater than the
replica count is harmless (replicas hold multiple shards each) but
wastes per-shard heartbeat overhead.

See [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}) for
the full STRATA_GC_SHARDS sizing table + leader-election shape.

## Helm

Out of scope this cycle. Helm chart packaging is queued as a P3
ROADMAP follow-up; today, apply the example manifests under
`deploy/k8s/` directly. Operators who already template against
their own Helm conventions can lift the manifest set into a chart in
~10 minutes — there is one Deployment, one Service, one ConfigMap, one
Secret, one Ingress.

## Apply test

The manifests under `deploy/k8s/` validate cleanly with
`kubectl apply --dry-run=client -f deploy/k8s/`. End-to-end apply
against a kind / minikube cluster:

```bash
kind create cluster --name strata-test
make docker-build                        # builds strata:ceph locally
kind load docker-image strata:ceph --name strata-test
# patch deploy/k8s/deployment.yaml to point image at strata:ceph (or use
# kustomize image override) before applying
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/configmap.yaml
kubectl apply -f deploy/k8s/secret.yaml
kubectl apply -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/service.yaml
kubectl -n strata rollout status deployment/strata
```

The example does not bundle TiKV / RADOS, so a kind apply against the
default ConfigMap will report `/readyz` as 503 until those externals
exist or you flip `STRATA_META_BACKEND=memory` + `STRATA_DATA_BACKEND=memory`
for a smoke pass.

## Production checklist

- [ ] Replicas ≥3 (anti-affinity per node; multi-AZ if the cluster spans zones).
- [ ] `STRATA_AUTH_MODE=required`. `STRATA_STATIC_CREDENTIALS` populated from a real secret store, not the placeholder.
- [ ] `STRATA_CONSOLE_JWT_SECRET` set in the Secret (avoid the JWT-shared-volume trap).
- [ ] `STRATA_GC_SHARDS` matches the replica count.
- [ ] Ingress preserves Host + buffers off; `STRATA_VHOST_PATTERN` matches the wildcard host.
- [ ] cert-manager `Certificate` with both apex + wildcard DNS names; HSTS enabled.
- [ ] PD ≥3, TiKV ≥3 (raft majority); RADOS pool `size=3`.
- [ ] Prometheus scrapes every pod (annotations on the Deployment template); alerts on `strata_worker_panic_total > 0`, `strata_replication_queue_age_seconds > <SLO>`, replica-count drift.
- [ ] OTel collector reachable from every pod. Sample ratio + ring buffer sized for traffic.
- [ ] Centralised log shipping draining JSON `stdout` (request_id + node_id are stamped on every line).
- [ ] PodSecurityStandards `restricted` enforced on the namespace; the manifest already complies.
- [ ] Disaster recovery runbook: TiKV PITR + RADOS pool snapshots tested end-to-end. Cross-region replicator worker (if applicable) configured.

## Cross-references

- [Single-node deployment]({{< ref "/deploy/single-node" >}}) — when one node is enough.
- [Docker Compose]({{< ref "/deploy/docker-compose" >}}) — reference compose stack the manifests are derived from.
- [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}) — STRATA_GC_SHARDS sizing, JWT secret distribution, leader-election shape.
- [Architecture — Backends — TiKV]({{< ref "/architecture/backends/tikv" >}}) — why TiKV is the recommended metadata backend for multi-replica.
- [Architecture — Storage]({{< ref "/architecture/storage" >}}) — sharded objects table, RADOS chunking, multi-replica scaling rationale.
