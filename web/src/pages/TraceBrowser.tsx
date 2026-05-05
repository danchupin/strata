import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useNavigate, useParams } from 'react-router-dom';
import {
  AlertCircle,
  Copy,
  ExternalLink,
  History,
  Search,
  X,
} from 'lucide-react';

import {
  fetchClusterStatus,
  fetchTrace,
  type TraceDoc,
  type TraceSpan,
} from '@/api/client';
import { queryKeys } from '@/lib/query';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

// RECENT_TRACES_KEY is a sessionStorage key holding the last RECENT_LIMIT
// resolved request-ids the operator pasted, newest first. Per AC.
const RECENT_TRACES_KEY = 'strata.diagnostics.recentTraces';
const RECENT_LIMIT = 10;
const ATTR_TRUNCATE_DB_STMT = 200;

interface RecentEntry {
  id: string;
  ts: number;
}

function loadRecent(): RecentEntry[] {
  try {
    const raw = sessionStorage.getItem(RECENT_TRACES_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as RecentEntry[];
    return Array.isArray(parsed) ? parsed.slice(0, RECENT_LIMIT) : [];
  } catch {
    return [];
  }
}

function pushRecent(id: string) {
  if (!id) return;
  const cur = loadRecent().filter((r) => r.id !== id);
  cur.unshift({ id, ts: Date.now() });
  try {
    sessionStorage.setItem(
      RECENT_TRACES_KEY,
      JSON.stringify(cur.slice(0, RECENT_LIMIT)),
    );
  } catch {
    // sessionStorage may be unavailable (privacy mode) — silently skip.
  }
}

// classifyService maps a span name to one of the colour buckets. Heuristic:
// - meta.cassandra.* / meta.tikv.* / meta.* → "meta"
// - data.rados.* / data.s3.* / data.* → "data"
// - everything else (HTTP root spans) → "gateway"
type ServiceKind = 'gateway' | 'meta' | 'data';
function classifyService(name: string): ServiceKind {
  if (name.startsWith('meta.')) return 'meta';
  if (name.startsWith('data.')) return 'data';
  return 'gateway';
}

const SERVICE_BAR_CLASS: Record<ServiceKind, string> = {
  gateway: 'bg-blue-500/70',
  meta: 'bg-emerald-500/70',
  data: 'bg-orange-500/70',
};
const SERVICE_LABEL: Record<ServiceKind, string> = {
  gateway: 'Gateway',
  meta: 'Meta',
  data: 'Data',
};

interface SpanLayoutRow {
  span: TraceSpan;
  depth: number;
  startNS: number;
  durNS: number;
}

// orderSpans builds a depth-first ordered list mirroring the tree shape.
// Spans without a known parent (or self-parent) become roots ordered by
// startTime. Children sort by startTime.
function orderSpans(spans: TraceSpan[]): SpanLayoutRow[] {
  if (spans.length === 0) return [];
  const byID = new Map<string, TraceSpan>();
  for (const s of spans) byID.set(s.span_id, s);

  const childrenOf = new Map<string, TraceSpan[]>();
  const roots: TraceSpan[] = [];
  for (const s of spans) {
    const parent = s.parent && byID.has(s.parent) ? s.parent : '';
    if (!parent) {
      roots.push(s);
      continue;
    }
    const arr = childrenOf.get(parent) ?? [];
    arr.push(s);
    childrenOf.set(parent, arr);
  }
  for (const arr of childrenOf.values()) {
    arr.sort((a, b) => a.start_ns - b.start_ns);
  }
  roots.sort((a, b) => a.start_ns - b.start_ns);

  const out: SpanLayoutRow[] = [];
  function visit(s: TraceSpan, depth: number) {
    out.push({
      span: s,
      depth,
      startNS: s.start_ns,
      durNS: Math.max(0, s.end_ns - s.start_ns),
    });
    const children = childrenOf.get(s.span_id) ?? [];
    for (const c of children) visit(c, depth + 1);
  }
  for (const r of roots) visit(r, 0);

  // Append orphans (parent set but missing) under depth 0 for visibility.
  if (out.length < spans.length) {
    const seen = new Set(out.map((r) => r.span.span_id));
    for (const s of spans) {
      if (!seen.has(s.span_id)) {
        out.push({
          span: s,
          depth: 0,
          startNS: s.start_ns,
          durNS: Math.max(0, s.end_ns - s.start_ns),
        });
      }
    }
  }
  return out;
}

function nsToMs(ns: number): number {
  return ns / 1_000_000;
}

function formatDuration(ns: number): string {
  if (ns <= 0) return '0 ms';
  const ms = nsToMs(ns);
  if (ms < 1) return `${(ms * 1000).toFixed(0)} µs`;
  if (ms < 10) return `${ms.toFixed(2)} ms`;
  if (ms < 1000) return `${ms.toFixed(1)} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

// HOVER_ATTRS lists the attribute keys surfaced in the on-bar tooltip per AC.
const HOVER_ATTRS = ['http.method', 'http.target', 'http.status_code', 'db.statement'] as const;

function attrToString(v: unknown, key?: string): string {
  if (v === null || v === undefined) return '';
  let s: string;
  if (typeof v === 'string') s = v;
  else if (typeof v === 'number' || typeof v === 'boolean') s = String(v);
  else s = JSON.stringify(v);
  if (key === 'db.statement' && s.length > ATTR_TRUNCATE_DB_STMT) {
    return `${s.slice(0, ATTR_TRUNCATE_DB_STMT)}…`;
  }
  return s;
}

function spanHoverText(span: TraceSpan): string {
  const parts: string[] = [span.name, `dur ${formatDuration(span.end_ns - span.start_ns)}`];
  if (span.status && span.status !== 'Unset') parts.push(`status ${span.status}`);
  for (const k of HOVER_ATTRS) {
    const v = span.attributes?.[k];
    if (v !== undefined && v !== null && v !== '') {
      parts.push(`${k}=${attrToString(v, k)}`);
    }
  }
  return parts.join(' · ');
}

interface TraceWaterfallProps {
  trace: TraceDoc;
  selectedID: string;
  onSelect: (span: TraceSpan) => void;
}

function TraceWaterfall({ trace, selectedID, onSelect }: TraceWaterfallProps) {
  const rows = useMemo(() => orderSpans(trace.spans), [trace.spans]);
  const range = useMemo(() => {
    if (rows.length === 0) return { startNS: 0, totalNS: 0 };
    let minStart = Number.POSITIVE_INFINITY;
    let maxEnd = 0;
    for (const r of rows) {
      if (r.startNS < minStart) minStart = r.startNS;
      const end = r.startNS + r.durNS;
      if (end > maxEnd) maxEnd = end;
    }
    return { startNS: minStart, totalNS: Math.max(1, maxEnd - minStart) };
  }, [rows]);

  if (rows.length === 0) {
    return (
      <div className="flex h-40 items-center justify-center text-sm text-muted-foreground">
        Trace has no spans.
      </div>
    );
  }

  return (
    <div className="space-y-1" role="list" aria-label="Trace waterfall">
      {rows.map((row) => {
        const offsetPct = ((row.startNS - range.startNS) / range.totalNS) * 100;
        const widthPct = Math.max(0.3, (row.durNS / range.totalNS) * 100);
        const service = classifyService(row.span.name);
        const isError = row.span.status === 'Error';
        const isSelected = selectedID === row.span.span_id;
        return (
          <button
            key={row.span.span_id}
            type="button"
            role="listitem"
            onClick={() => onSelect(row.span)}
            title={spanHoverText(row.span)}
            className={cn(
              'group flex w-full items-center gap-2 rounded-sm px-1 py-0.5 text-left transition-colors hover:bg-muted/40',
              isSelected && 'bg-muted/60 ring-1 ring-primary/40',
            )}
            aria-pressed={isSelected}
          >
            <span
              className="truncate text-xs font-mono"
              style={{ paddingLeft: `${row.depth * 16}px`, minWidth: '12rem', maxWidth: '20rem' }}
            >
              {row.span.name}
            </span>
            <span className="relative h-4 flex-1 rounded-sm bg-muted/30">
              <span
                className={cn(
                  'absolute top-0 h-full rounded-sm',
                  SERVICE_BAR_CLASS[service],
                  isError && 'ring-1 ring-red-500',
                )}
                style={{ left: `${offsetPct}%`, width: `${widthPct}%` }}
                aria-hidden
              />
            </span>
            <span className="w-20 shrink-0 text-right font-mono text-xs tabular-nums text-muted-foreground">
              {formatDuration(row.durNS)}
            </span>
          </button>
        );
      })}
    </div>
  );
}

interface SpanDetailSheetProps {
  span: TraceSpan | null;
  trace: TraceDoc | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

function SpanDetailSheet({ span, trace, open, onOpenChange }: SpanDetailSheetProps) {
  const attrs = span?.attributes ?? {};
  const attrEntries = Object.entries(attrs).sort(([a], [b]) => a.localeCompare(b));

  function handleCopy() {
    if (!trace) return;
    void navigator.clipboard
      .writeText(JSON.stringify(trace, null, 2))
      .then(() =>
        showToast({
          title: 'Trace copied',
          description: `${trace.spans.length} spans (trace_id ${trace.trace_id.slice(0, 8)}…)`,
        }),
      )
      .catch((err: unknown) => {
        showToast({
          title: 'Copy failed',
          description: err instanceof Error ? err.message : 'clipboard unavailable',
          variant: 'destructive',
        });
      });
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="w-full overflow-y-auto sm:max-w-lg"
        aria-describedby="trace-span-desc"
      >
        <SheetHeader>
          <SheetTitle className="font-mono text-sm">{span?.name ?? 'Span detail'}</SheetTitle>
          <SheetDescription id="trace-span-desc">
            Span attributes, timing, and status.
          </SheetDescription>
        </SheetHeader>
        {span && (
          <div className="mt-4 space-y-4 text-sm">
            <div className="grid grid-cols-2 gap-3 text-xs">
              <div>
                <div className="text-muted-foreground">Span ID</div>
                <div className="font-mono">{span.span_id}</div>
              </div>
              <div>
                <div className="text-muted-foreground">Parent</div>
                <div className="font-mono">{span.parent || '—'}</div>
              </div>
              <div>
                <div className="text-muted-foreground">Duration</div>
                <div className="font-mono">{formatDuration(span.end_ns - span.start_ns)}</div>
              </div>
              <div>
                <div className="text-muted-foreground">Status</div>
                <div
                  className={cn(
                    'font-mono',
                    span.status === 'Error' && 'text-red-600 dark:text-red-400',
                    span.status === 'OK' && 'text-emerald-600 dark:text-emerald-400',
                  )}
                >
                  {span.status || 'Unset'}
                </div>
              </div>
              <div>
                <div className="text-muted-foreground">Service</div>
                <div className="font-mono">{SERVICE_LABEL[classifyService(span.name)]}</div>
              </div>
              <div>
                <div className="text-muted-foreground">Start (ns)</div>
                <div className="font-mono tabular-nums">{span.start_ns}</div>
              </div>
            </div>

            <div>
              <div className="mb-2 text-xs font-medium text-muted-foreground">
                Attributes ({attrEntries.length})
              </div>
              {attrEntries.length === 0 ? (
                <div className="text-xs text-muted-foreground">No attributes.</div>
              ) : (
                <table className="w-full table-fixed text-xs">
                  <tbody className="divide-y divide-border">
                    {attrEntries.map(([k, v]) => (
                      <tr key={k}>
                        <td className="w-1/3 truncate py-1.5 pr-2 align-top font-mono text-muted-foreground">
                          {k}
                        </td>
                        <td className="break-all py-1.5 align-top font-mono">
                          {attrToString(v, k)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>

            <div className="flex justify-end pt-2">
              <Button type="button" size="sm" variant="outline" onClick={handleCopy}>
                <Copy className="mr-1.5 h-3.5 w-3.5" aria-hidden /> Copy trace JSON
              </Button>
            </div>
          </div>
        )}
      </SheetContent>
    </Sheet>
  );
}

export function TraceBrowserPage() {
  const params = useParams<{ requestID?: string }>();
  const navigate = useNavigate();
  const routeID = params.requestID ?? '';
  const [pasted, setPasted] = useState('');
  const [recent, setRecent] = useState<RecentEntry[]>(() => loadRecent());
  const [selectedSpanID, setSelectedSpanID] = useState<string>('');
  const [sheetOpen, setSheetOpen] = useState(false);

  const traceQ = useQuery({
    enabled: routeID !== '',
    queryKey: queryKeys.diagnostics.trace(routeID),
    queryFn: () => fetchTrace(routeID),
    meta: { label: 'trace', silent: true },
  });

  const statusQ = useQuery({
    queryKey: queryKeys.cluster.status,
    queryFn: fetchClusterStatus,
    meta: { silent: true },
  });
  const otelEndpoint = statusQ.data?.otel_endpoint ?? '';

  // Promote a successful resolution to the recent-list (and drop a no-op
  // when the operator re-views the same id back to back).
  useEffect(() => {
    if (routeID && traceQ.data) {
      pushRecent(routeID);
      setRecent(loadRecent());
    }
  }, [routeID, traceQ.data]);

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = pasted.trim();
    if (!trimmed) return;
    navigate(`/diagnostics/trace/${encodeURIComponent(trimmed)}`);
    setPasted('');
  }

  function clearRecent() {
    try {
      sessionStorage.removeItem(RECENT_TRACES_KEY);
    } catch {
      // ignore
    }
    setRecent([]);
  }

  function handleSelectSpan(span: TraceSpan) {
    setSelectedSpanID(span.span_id);
    setSheetOpen(true);
  }

  const trace = traceQ.data ?? null;
  const showSkeleton = routeID !== '' && traceQ.isPending;
  const notFound = routeID !== '' && !traceQ.isPending && !trace && !traceQ.error;
  const errorMessage =
    routeID !== '' && traceQ.error instanceof Error ? traceQ.error.message : null;
  const totalNS = useMemo(() => {
    if (!trace || trace.spans.length === 0) return 0;
    let min = Number.POSITIVE_INFINITY;
    let max = 0;
    for (const s of trace.spans) {
      if (s.start_ns < min) min = s.start_ns;
      if (s.end_ns > max) max = s.end_ns;
    }
    return Math.max(0, max - min);
  }, [trace]);

  const selectedSpan = useMemo(() => {
    if (!trace || !selectedSpanID) return null;
    return trace.spans.find((s) => s.span_id === selectedSpanID) ?? null;
  }, [trace, selectedSpanID]);

  const jaegerHref = useMemo(() => {
    if (!otelEndpoint || !trace) return '';
    // OTLP collector endpoints look like https://collector.example:4318;
    // Jaeger UI usually sits on the same host with a different path. We
    // strip the OTLP path/port and append /trace/<id> so operators with a
    // single-host all-in-one (default Jaeger compose ships UI on :16686)
    // get a usable link. The deploy/docker recipe documents the override.
    try {
      const u = new URL(otelEndpoint);
      const host = u.hostname;
      const proto = u.protocol || 'http:';
      return `${proto}//${host}:16686/trace/${trace.trace_id}`;
    } catch {
      return `${otelEndpoint.replace(/\/$/, '')}/trace/${trace.trace_id}`;
    }
  }, [otelEndpoint, trace]);

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Trace browser</h1>
        <p className="text-sm text-muted-foreground">
          In-process OTel ring buffer (~10 minutes retention). Paste an{' '}
          <span className="font-mono">X-Request-Id</span> the gateway returned
          on a response, or a raw trace id.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Look up a trace</CardTitle>
          <CardDescription>
            Resolves by request id first; falls back to trace id when the
            string is a 32-hex value.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="flex flex-col gap-2 sm:flex-row sm:items-end">
            <div className="flex-1">
              <Label htmlFor="trace-input" className="text-xs">
                Paste a request-id
              </Label>
              <Input
                id="trace-input"
                value={pasted}
                onChange={(e) => setPasted(e.target.value)}
                placeholder="e.g. 4f1c0e96-…"
                className="font-mono"
                autoFocus
              />
            </div>
            <Button type="submit" disabled={!pasted.trim()}>
              <Search className="mr-1.5 h-4 w-4" aria-hidden /> Look up
            </Button>
          </form>

          {recent.length > 0 && (
            <div className="mt-4">
              <div className="mb-1 flex items-center justify-between">
                <Label className="text-xs text-muted-foreground">
                  <History className="mr-1 inline h-3 w-3" aria-hidden /> Recent (this session)
                </Label>
                <button
                  type="button"
                  onClick={clearRecent}
                  className="inline-flex items-center text-xs text-muted-foreground underline-offset-2 hover:underline"
                  aria-label="Clear recent traces"
                >
                  <X className="mr-0.5 h-3 w-3" aria-hidden /> Clear
                </button>
              </div>
              <ul className="flex flex-wrap gap-1">
                {recent.map((r) => (
                  <li key={`${r.id}-${r.ts}`}>
                    <button
                      type="button"
                      onClick={() => navigate(`/diagnostics/trace/${encodeURIComponent(r.id)}`)}
                      className={cn(
                        'rounded-md border border-input bg-background px-2 py-0.5 font-mono text-xs hover:bg-muted',
                        r.id === routeID && 'border-primary text-primary',
                      )}
                      title={r.id}
                    >
                      {r.id.length > 18 ? `${r.id.slice(0, 8)}…${r.id.slice(-6)}` : r.id}
                    </button>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </CardContent>
      </Card>

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load trace</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      {showSkeleton && (
        <Card>
          <CardContent className="space-y-2 py-6">
            <Skeleton className="h-5 w-1/2" />
            <Skeleton className="h-32 w-full" />
          </CardContent>
        </Card>
      )}

      {notFound && (
        <Card>
          <CardContent className="space-y-2 py-10 text-center">
            <div className="text-sm font-medium">Trace not retained</div>
            <div className="text-xs text-muted-foreground">
              <span className="font-mono">{routeID}</span> is unknown to the in-process
              ring buffer. The trace may have aged out (~10 min retention) or never
              been sampled.
            </div>
          </CardContent>
        </Card>
      )}

      {trace && (
        <Card>
          <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0">
            <div>
              <CardTitle className="text-base">
                {trace.spans.length} span{trace.spans.length === 1 ? '' : 's'} ·{' '}
                {formatDuration(totalNS)}
              </CardTitle>
              <CardDescription className="font-mono text-xs">
                trace_id {trace.trace_id}
                {trace.request_id ? ` · request_id ${trace.request_id}` : ''}
              </CardDescription>
            </div>
            {otelEndpoint && jaegerHref && (
              <a
                href={jaegerHref}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 rounded-md border border-input bg-background px-2 py-1 text-xs hover:bg-muted"
                title="Opens the configured Jaeger UI in a new tab"
              >
                Open in Jaeger
                <ExternalLink className="h-3 w-3" aria-hidden />
              </a>
            )}
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
              <span className="inline-flex items-center gap-1">
                <span className="h-2.5 w-2.5 rounded-sm bg-blue-500/70" aria-hidden />
                Gateway
              </span>
              <span className="inline-flex items-center gap-1">
                <span className="h-2.5 w-2.5 rounded-sm bg-emerald-500/70" aria-hidden />
                Meta
              </span>
              <span className="inline-flex items-center gap-1">
                <span className="h-2.5 w-2.5 rounded-sm bg-orange-500/70" aria-hidden />
                Data
              </span>
              <span className="inline-flex items-center gap-1">
                <span className="h-2.5 w-2.5 rounded-sm bg-muted ring-1 ring-red-500" aria-hidden />
                Errored
              </span>
            </div>
            <TraceWaterfall
              trace={trace}
              selectedID={selectedSpanID}
              onSelect={handleSelectSpan}
            />
          </CardContent>
        </Card>
      )}

      <SpanDetailSheet
        span={selectedSpan}
        trace={trace}
        open={sheetOpen}
        onOpenChange={(open) => {
          setSheetOpen(open);
          if (!open) setSelectedSpanID('');
        }}
      />
    </div>
  );
}
