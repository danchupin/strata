---
title: 'Get Started'
weight: 10
bookFlatSection: true
description: 'Bring up a local Strata gateway and put your first object in under 5 minutes.'
---

# Get Started

A 5-minute quick start lands in US-004. Until then, the canonical bootstrap
path is:

```bash
make run-memory   # in-memory meta + data; no Docker
```

…and then point any S3 client at `http://localhost:8080`.

Next: [Architecture]({{< ref "/architecture" >}}) for the layer-by-layer
deep dive, [Deploy]({{< ref "/deploy" >}}) for production shapes.
