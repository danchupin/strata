---
title: 'Operate'
weight: 25
bookFlatSection: true
description: 'Day-2 ops workflows — drain a cluster, monitor, scale, back up.'
---

# Operate

Day-2 operator playbook for running Strata. Each page is a workflow:
the steps an operator takes from the LB or the admin API to keep the
cluster healthy, drain a member, plan capacity, or recover from a
backup. Implementation details and tuning rationale live in
[Best Practices]({{< relref "/best-practices/" >}}) and
[Architecture]({{< relref "/architecture/" >}}); this section answers
"what do I run, in what order, and what do I watch?"

{{% columns %}}
- {{< card href="/operate/drain-cluster/" >}}
  **Drain a cluster**  
  Step-by-step decommission workflow — `/admin/v1/clusters/{id}/drain`,
  watch live ETA + bandwidth chips, wait for deregister-ready, remove
  from `STRATA_RADOS_CLUSTERS`.
  {{< /card >}}

- {{< card href="/operate/monitoring/" >}}
  **Monitoring**  
  Prometheus scrape, key metrics, Grafana dashboard, OTel collector +
  Jaeger wire-up, in-process trace browser, audit log retention.
  {{< /card >}}

- {{< card href="/operate/scaling/" >}}
  **Scaling**  
  CPU / RAM per replica, metadata-tier sizing pointers, when to scale
  out vs scale up the gateway tier or the meta tier.
  {{< /card >}}
{{% /columns %}}

{{% columns %}}
- {{< card href="/operate/backup-restore/" >}}
  **Backup + restore**  
  Snapshot strategy across metadata (Cassandra / TiKV), data (RADOS / S3),
  and the replicator + inventory + audit-export workers.
  {{< /card >}}

- {{< card href="/operate/capacity-planning/" >}}
  **Capacity planning**  
  Chunk fan-out math, lifecycle drain rate vs PUT rate, when to scale
  bucket shards, dedup roadmap math.
  {{< /card >}}

- {{< card href="/operate/profiling/" >}}
  **Profiling**  
  Opt-in `/debug/pprof/*` behind admin auth — heap, CPU, goroutine,
  block, mutex, trace recipes for perf incident response.
  {{< /card >}}
{{% /columns %}}

{{% columns %}}
- {{< card href="/operate/slo/" >}}
  **SLO / SLI**  
  Availability / latency / durability baselines + `strata admin
  slo-report` weekly compliance workflow.
  {{< /card >}}
{{% /columns %}}

## See also

- [Get Started]({{< relref "/get-started/" >}}) — first install and
  smoke run.
- [Deploy]({{< relref "/deploy/" >}}) — deployment shapes (single
  node, docker compose, multi-replica, Kubernetes).
- [Best Practices]({{< relref "/best-practices/" >}}) — tuning knobs
  and runbooks for placement, tracing, GC, quotas, multi-cluster.
- [Architecture]({{< relref "/architecture/" >}}) — implementation
  rationale for every operate-facing knob.
