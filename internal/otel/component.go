package otel

import "go.opentelemetry.io/otel/attribute"

// AttrComponentKey is the attribute key used to discriminate gateway-side
// spans (HTTP middleware, Cassandra/TiKV meta observers, RADOS/S3 data
// observers) from worker-side spans (per-iteration parent + sub-op
// children). Operators filter Jaeger by this attribute to scope a query
// to request-path or background activity.
const AttrComponentKey = "strata.component"

// AttrComponentGateway stamps the gateway component label on every span
// emitted by gateway-side observers. Reused by HTTP middleware,
// Cassandra/TiKV meta observers, RADOS/S3 data observers so callers do
// not duplicate the magic string.
var AttrComponentGateway = attribute.String(AttrComponentKey, "gateway")

// AttrComponentWorker stamps the worker component label on every span
// emitted by background workers (per-iteration parent + sub-op children).
// Workers acquire a tracer via deps.Tracer.Tracer("strata.worker.<name>")
// and stamp this attribute on the per-iteration parent via the
// StartIteration / EndIteration helper.
var AttrComponentWorker = attribute.String(AttrComponentKey, "worker")
