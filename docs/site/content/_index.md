---
title: 'Strata Documentation'
type: 'docs'
weight: 1
bookFlatSection: true
bookToc: false
description: 'S3-compatible object gateway. Cassandra/TiKV metadata, RADOS data. Drop-in replacement for Ceph RGW.'
---

<style>
.strata-hero {
  margin: 1rem 0 2.5rem;
  padding: 2rem 1.5rem;
  border-radius: 8px;
  background: var(--gray-100);
  border: 1px solid var(--gray-200);
}
.strata-hero h1 {
  margin-top: 0;
  font-size: 2.4rem;
  line-height: 1.15;
}
.strata-hero p.lede {
  font-size: 1.15rem;
  color: var(--body-font-color);
  max-width: 48rem;
  margin: 0.5rem 0 1.5rem;
}
.strata-cta {
  display: inline-block;
  padding: 0.65rem 1.2rem;
  margin: 0.25rem 0.5rem 0.25rem 0;
  border-radius: 6px;
  font-weight: 600;
  text-decoration: none !important;
  border: 1px solid var(--color-link);
}
.strata-cta-primary {
  background: var(--color-link);
  color: var(--body-background) !important;
}
.strata-cta-secondary {
  color: var(--color-link) !important;
  background: transparent;
}
.strata-feature-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
  gap: 1rem;
  margin: 1.5rem 0 2rem;
}
.strata-feature-card {
  padding: 1.1rem 1.2rem;
  border-radius: 6px;
  border: 1px solid var(--gray-200);
  background: var(--body-background);
  display: flex;
  flex-direction: column;
}
.strata-feature-card h3 {
  margin: 0 0 0.4rem;
  font-size: 1.05rem;
}
.strata-feature-card p {
  margin: 0 0 0.7rem;
  color: var(--gray-500);
  font-size: 0.95rem;
  flex: 1;
}
.strata-feature-card a.feature-link {
  color: var(--color-link);
  font-size: 0.92rem;
  text-decoration: none;
  font-weight: 500;
}
.strata-feature-card a.feature-link:hover { text-decoration: underline; }
.strata-positioning {
  padding: 1.2rem 1.4rem;
  border-left: 3px solid var(--color-link);
  background: var(--gray-100);
  margin: 1.5rem 0;
  border-radius: 0 6px 6px 0;
}
.strata-positioning h2 { margin-top: 0; }
.strata-footer-links {
  margin: 2rem 0 1rem;
  padding-top: 1rem;
  border-top: 1px solid var(--gray-200);
  color: var(--gray-500);
  font-size: 0.92rem;
}
.strata-footer-links a { color: var(--color-link); margin-right: 1rem; }
</style>

<div class="strata-hero">
  <h1>Strata</h1>
  <p class="lede">
    S3-compatible object gateway, written in Go. Metadata in Cassandra,
    ScyllaDB, or TiKV. Data as 4&nbsp;MiB chunks in RADOS or any S3 bucket.
    Drop-in replacement for Ceph RGW — without the bucket-index ceiling.
  </p>
  <a class="strata-cta strata-cta-primary" href="{{< relref "/get-started/" >}}">Get Started →</a>
  <a class="strata-cta strata-cta-secondary" href="{{< relref "/architecture/" >}}">Architecture</a>
</div>

## Why Strata

<div class="strata-feature-grid">

  <div class="strata-feature-card">
    <h3>Sharded objects table</h3>
    <p>The metadata layer fans out by <code>hash(key) % N</code>, dodging the bucket-index ceiling that bites RGW at scale.</p>
    <a class="feature-link" href="{{< relref "/architecture/" >}}">Sharding & fan-out →</a>
  </div>

  <div class="strata-feature-card">
    <h3>TiKV ordered range scans</h3>
    <p>Native ordered scans on TiKV short-circuit the 64-way Cassandra fan-out via <code>RangeScanStore</code>. List-heavy workloads love it.</p>
    <a class="feature-link" href="{{< relref "/architecture/backends/tikv" >}}">TiKV backend →</a>
  </div>

  <div class="strata-feature-card">
    <h3>Multi-replica scaling</h3>
    <p>Run N gateway replicas with per-shard leader election. <code>STRATA_GC_SHARDS</code> sizes the GC fan-out to match your cluster.</p>
    <a class="feature-link" href="{{< relref "/architecture/migrations/gc-lifecycle-phase-2" >}}">GC fan-out (Phase 2) →</a>
  </div>

  <div class="strata-feature-card">
    <h3>Online bucket reshard</h3>
    <p>Resize a bucket's shard count without taking it offline. Reshard worker rebalances rows in the background.</p>
    <a class="feature-link" href="{{< relref "/architecture/" >}}">Reshard worker →</a>
  </div>

  <div class="strata-feature-card">
    <h3>Multi-cluster RADOS routing</h3>
    <p>Route data to multiple Ceph clusters. Per-pool storage classes; lifecycle transitions move bytes between tiers.</p>
    <a class="feature-link" href="{{< relref "/architecture/storage" >}}">Storage backends →</a>
  </div>

  <div class="strata-feature-card">
    <h3>Embedded operator console</h3>
    <p>Bundled Web UI for buckets, IAM, lifecycle, audit log, traces. No separate console binary, no extra deploy.</p>
    <a class="feature-link" href="{{< relref "/best-practices/web-ui" >}}">Web UI →</a>
  </div>

  <div class="strata-feature-card">
    <h3>OpenTelemetry tracing</h3>
    <p>OTLP exporter with tail-sampling — failing spans always export. In-process ring buffer feeds a per-request trace browser in the console.</p>
    <a class="feature-link" href="{{< relref "/architecture/" >}}">Observability →</a>
  </div>

  <div class="strata-feature-card">
    <h3>Drop-in Ceph RGW replacement</h3>
    <p>SigV4, presigned URLs, multipart, versioning, lifecycle, ACL, Object Lock, CORS — measured against Ceph's <code>s3-tests</code> suite.</p>
    <a class="feature-link" href="{{< relref "/s3-compatibility/" >}}">S3 compatibility →</a>
  </div>

</div>

<div class="strata-positioning">

## What Strata is (and isn't)

Strata is a thin S3 frontend over a metadata store and a chunk store. It is a
**drop-in replacement for Ceph RGW** when RGW's bucket index is the bottleneck,
and a **lighter alternative to MinIO** when you already run Cassandra, ScyllaDB,
or TiKV. It is **not** a full distributed filesystem like SeaweedFS, and it
does not ship its own data plane — RADOS or another S3 bucket holds the bytes.
Per-feature support is tracked against `s3-tests` on the
[S3 Compatibility]({{< relref "/s3-compatibility/" >}}) page.

</div>

<div class="strata-footer-links">
  <a href="https://github.com/danchupin/strata">GitHub</a>
  <a href="https://github.com/danchupin/strata/blob/main/ROADMAP.md">ROADMAP</a>
  <a href="https://github.com/danchupin/strata/blob/main/CLAUDE.md">CLAUDE.md (devs)</a>
</div>
