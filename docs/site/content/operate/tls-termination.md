---
title: TLS termination + backend mTLS
weight: 50
draft: true
---

> Stub created in US-006 of `ralph/harden-gateway`. US-010 fills in the
> four deploy shapes (plain HTTP behind ingress / Strata-terminated
> single-port / Strata-terminated split admin+S3 / Strata-terminated +
> backend mTLS), cert-provisioning recipes (cert-manager / Vault PKI /
> openssl), and per-backend mTLS sections (Cassandra / TiKV / S3-upstream).
> Removes `draft: true` on cycle close.

## RADOS uses cephx, not TLS

Strata does **not** ship an mTLS layer for the RADOS data backend.
`librados` authenticates via Ceph's native **cephx** mutual-auth protocol,
which is the production-supported path: clients prove possession of the
keyring (`STRATA_RADOS_KEYRING` / `[rados].keyring`) to the MON cluster on
every connection, and the MON cluster signs the OSD-level capability tokens
that gate every read + write. Adding a TLS layer on top would re-encrypt an
already-mutually-authenticated channel.

If you need wire-level confidentiality on a public-network Ceph cluster,
configure `ms_cluster_mode=secure` + `ms_client_mode=secure` in your
`ceph.conf` — the cluster-managed cephx handshake then encrypts the
messenger payload end-to-end. See the upstream
[Ceph networking](https://docs.ceph.com/en/latest/rados/configuration/msgr2/)
notes; no Strata-side knob is required.

## Other backends

- Cassandra mTLS — `STRATA_CASSANDRA_TLS_*` (US-004; see [env vars]({{< ref "/reference/env-vars" >}}#data-backend--cassandra)).
- TiKV mTLS — `STRATA_TIKV_TLS_*` (US-005; see [env vars]({{< ref "/reference/env-vars" >}}#meta-backend--tikv)).
- S3-upstream mTLS — `STRATA_S3_TLS_*` global default with per-cluster
  `tls` override on `STRATA_S3_CLUSTERS` (US-006; see [env vars]({{< ref "/reference/env-vars" >}}#data-backend--s3-pass-through)).
