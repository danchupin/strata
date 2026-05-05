import { useEffect, useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import {
  BarChart,
  Bar,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { AlertCircle, ExternalLink, RefreshCw } from 'lucide-react';

import {
  fetchBucketsList,
  fetchSlowQueries,
  type SlowQueryRow,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { cn } from '@/lib/utils';

const POLL_INTERVAL_MS = 30_000;
const FILTER_DEBOUNCE_MS = 300;

type RangeKey = '15m' | '1h' | '6h' | '24h' | '7d';
const RANGE_OPTIONS: Array<{ key: RangeKey; label: string }> = [
  { key: '15m', label: '15m' },
  { key: '1h', label: '1h' },
  { key: '6h', label: '6h' },
  { key: '24h', label: '24h' },
  { key: '7d', label: '7d' },
];

type OpKey = 'GET' | 'PUT' | 'DELETE' | 'HEAD' | 'Multipart';
const OP_OPTIONS: OpKey[] = ['GET', 'PUT', 'DELETE', 'HEAD', 'Multipart'];

// classifyOp maps an audit `action` string to one of the OpKey buckets so the
// client-side Op filter can match. Multipart wins over PUT/DELETE if the
// action mentions either UploadPart, CreateMultipartUpload,
// CompleteMultipartUpload, or AbortMultipartUpload.
function classifyOp(action: string): OpKey | null {
  if (!action) return null;
  if (
    action.includes('MultipartUpload') ||
    action.includes('UploadPart')
  ) {
    return 'Multipart';
  }
  // strip an `admin:` prefix so admin verbs categorise alongside their
  // user-facing counterparts (admin:DeleteIAMUser → DELETE).
  const bare = action.startsWith('admin:') ? action.slice('admin:'.length) : action;
  if (bare.startsWith('Get')) return 'GET';
  if (bare.startsWith('Put') || bare.startsWith('Set') || bare.startsWith('Create') || bare.startsWith('Update') || bare.startsWith('Attach')) {
    return 'PUT';
  }
  if (bare.startsWith('Delete') || bare.startsWith('Detach') || bare.startsWith('Abort')) return 'DELETE';
  if (bare.startsWith('Head')) return 'HEAD';
  return null;
}

function useDebounced<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}

// rangeToDuration converts a UI range key to the Go duration string the
// backend `since` parameter accepts.
function rangeToDuration(r: RangeKey): string {
  switch (r) {
    case '15m':
      return '15m';
    case '1h':
      return '1h';
    case '6h':
      return '6h';
    case '24h':
      return '24h';
    case '7d':
      return '168h';
  }
}

function latencyBadgeClass(ms: number): string {
  if (ms >= 1000) return 'bg-red-500/10 text-red-700 dark:text-red-300 border-red-500/30';
  if (ms >= 500) return 'bg-orange-500/10 text-orange-700 dark:text-orange-300 border-orange-500/30';
  if (ms >= 100) return 'bg-yellow-500/10 text-yellow-700 dark:text-yellow-300 border-yellow-500/30';
  return 'bg-muted text-muted-foreground border-border';
}

function shortenID(id: string, head = 8, tail = 4): string {
  if (id.length <= head + tail + 1) return id;
  return `${id.slice(0, head)}…${id.slice(-tail)}`;
}

// Histogram buckets — log-style, aligned to the order of magnitudes operators
// reason about for HTTP latency. Per PRD AC: 8 buckets, 0-10ms … ≥10s.
const HISTOGRAM_BUCKETS: Array<{ label: string; min: number; max: number }> = [
  { label: '0–10ms', min: 0, max: 10 },
  { label: '10–50ms', min: 10, max: 50 },
  { label: '50–100ms', min: 50, max: 100 },
  { label: '100–500ms', min: 100, max: 500 },
  { label: '500ms–1s', min: 500, max: 1_000 },
  { label: '1–5s', min: 1_000, max: 5_000 },
  { label: '5–10s', min: 5_000, max: 10_000 },
  { label: '≥10s', min: 10_000, max: Number.POSITIVE_INFINITY },
];

function bucketRows(rows: SlowQueryRow[]): Array<{ bucket: string; count: number }> {
  const counts = HISTOGRAM_BUCKETS.map(() => 0);
  for (const r of rows) {
    for (let i = 0; i < HISTOGRAM_BUCKETS.length; i++) {
      const b = HISTOGRAM_BUCKETS[i];
      if (r.latency_ms >= b.min && r.latency_ms < b.max) {
        counts[i]++;
        break;
      }
    }
  }
  return HISTOGRAM_BUCKETS.map((b, i) => ({ bucket: b.label, count: counts[i] }));
}

export function SlowQueriesPage() {
  const [rangeKey, setRangeKey] = useState<RangeKey>('1h');
  const [minMsInput, setMinMsInput] = useState('100');
  const [bucket, setBucket] = useState('');
  const [opsSelected, setOpsSelected] = useState<Set<OpKey>>(new Set());
  const [pageTokens, setPageTokens] = useState<string[]>(['']);

  const since = rangeToDuration(rangeKey);
  const debouncedBucket = useDebounced(bucket.trim(), FILTER_DEBOUNCE_MS);
  const minMs = useMemo(() => {
    const parsed = parseInt(minMsInput, 10);
    if (!Number.isFinite(parsed) || parsed < 0) return 0;
    return parsed;
  }, [minMsInput]);

  // Reset pagination when any filter changes.
  useEffect(() => {
    setPageTokens(['']);
  }, [rangeKey, minMs, debouncedBucket, opsSelected]);

  const currentToken = pageTokens[pageTokens.length - 1] ?? '';

  const bucketsQ = useQuery({
    queryKey: ['buckets', 'list', { query: '', sort: '', order: 'asc', page: 1, pageSize: 1000 }],
    queryFn: () => fetchBucketsList({ pageSize: 1000 }),
    meta: { silent: true },
  });
  const bucketSuggestions = useMemo(
    () => (bucketsQ.data?.buckets ?? []).map((b) => b.name).sort(),
    [bucketsQ.data],
  );

  const q = useQuery({
    queryKey: queryKeys.diagnostics.slowQueries(since, minMs, currentToken),
    queryFn: () => fetchSlowQueries({ since, minMs, pageToken: currentToken || undefined }),
    refetchInterval: POLL_INTERVAL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'slow queries' },
  });

  const rawRows = q.data?.rows ?? [];
  // Bucket + Op filtering is client-side — the backend endpoint exposes only
  // since/min_ms/page_token. The page-token continuation reflects the wire
  // pagination (server-sorted by latency desc); client-side filtering only
  // narrows the visible page.
  const rows = useMemo(() => {
    return rawRows.filter((r) => {
      if (debouncedBucket && r.bucket !== debouncedBucket) return false;
      if (opsSelected.size > 0) {
        const op = classifyOp(r.op);
        if (!op || !opsSelected.has(op)) return false;
      }
      return true;
    });
  }, [rawRows, debouncedBucket, opsSelected]);
  const nextToken = q.data?.next_page_token ?? '';
  const showSkeleton = q.isPending && !q.data;
  const errorMessage = !q.data && q.error instanceof Error ? q.error.message : null;

  const histogram = useMemo(() => bucketRows(rows), [rows]);

  function toggleOp(op: OpKey) {
    setOpsSelected((prev) => {
      const next = new Set(prev);
      if (next.has(op)) next.delete(op);
      else next.add(op);
      return next;
    });
  }

  function handleClearOps() {
    setOpsSelected(new Set());
  }

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: ['diagnostics', 'slow-queries'] });
  }

  function handleLoadMore() {
    if (!nextToken) return;
    setPageTokens((prev) => [...prev, nextToken]);
  }

  function handleResetPaging() {
    setPageTokens(['']);
  }

  const pageNumber = pageTokens.length;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Slow queries</h1>
        <p className="text-sm text-muted-foreground">
          Last <span className="font-mono">N</span> requests above the latency
          threshold. Reads from the audit log; GET / HEAD are never audited so
          they will not appear regardless of Op filter.
        </p>
      </div>

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load slow queries</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Filters</CardTitle>
          <CardDescription>
            Server-side filters: time window + min latency. Bucket + Op are
            applied client-side to the current page.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <div>
            <Label className="text-xs">Time window</Label>
            <div
              role="group"
              aria-label="Time window"
              className="inline-flex rounded-md border border-input bg-background p-0.5 text-sm"
            >
              {RANGE_OPTIONS.map((opt) => (
                <button
                  key={opt.key}
                  type="button"
                  onClick={() => setRangeKey(opt.key)}
                  aria-pressed={rangeKey === opt.key}
                  className={cn(
                    'rounded-sm px-2.5 py-1 font-medium transition-colors',
                    rangeKey === opt.key
                      ? 'bg-primary text-primary-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  {opt.label}
                </button>
              ))}
            </div>
          </div>
          <div>
            <Label className="text-xs" htmlFor="slow-filter-min-ms">
              Min latency (ms)
            </Label>
            <Input
              id="slow-filter-min-ms"
              type="number"
              min={0}
              value={minMsInput}
              onChange={(e) => setMinMsInput(e.target.value)}
              placeholder="100"
            />
          </div>
          <div>
            <Label className="text-xs" htmlFor="slow-filter-bucket">
              Bucket
            </Label>
            <Input
              id="slow-filter-bucket"
              value={bucket}
              onChange={(e) => setBucket(e.target.value)}
              placeholder="any"
              list="slow-filter-bucket-suggestions"
            />
            <datalist id="slow-filter-bucket-suggestions">
              {bucketSuggestions.map((b) => (
                <option key={b} value={b} />
              ))}
            </datalist>
          </div>
          <div>
            <Label className="text-xs">Op</Label>
            <div className="flex flex-wrap items-center gap-1">
              {OP_OPTIONS.map((op) => {
                const checked = opsSelected.has(op);
                return (
                  <button
                    key={op}
                    type="button"
                    aria-pressed={checked}
                    onClick={() => toggleOp(op)}
                    className={cn(
                      'rounded-md border px-2 py-0.5 text-xs font-medium transition-colors',
                      checked
                        ? 'border-primary bg-primary text-primary-foreground'
                        : 'border-input bg-background text-muted-foreground hover:text-foreground',
                    )}
                  >
                    {op === 'Multipart' ? 'Multipart*' : op}
                  </button>
                );
              })}
              {opsSelected.size > 0 && (
                <button
                  type="button"
                  onClick={handleClearOps}
                  className="text-xs underline-offset-2 hover:underline"
                >
                  clear
                </button>
              )}
            </div>
          </div>
        </CardContent>
      </Card>

      <Tabs defaultValue="table">
        <div className="flex items-center justify-between gap-3">
          <TabsList>
            <TabsTrigger value="table">Rows</TabsTrigger>
            <TabsTrigger value="histogram">Latency histogram</TabsTrigger>
          </TabsList>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={handleRefresh}
            disabled={q.isFetching}
            aria-label="Refresh slow queries"
          >
            <RefreshCw
              className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
              aria-hidden
            />
            Refresh
          </Button>
        </div>

        <TabsContent value="table" className="mt-4">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Slowest requests</CardTitle>
              <CardDescription>
                {q.isFetching && !showSkeleton
                  ? 'Refreshing…'
                  : `Page ${pageNumber} · ${rows.length} ${rows.length === 1 ? 'row' : 'rows'} · auto-refresh 30 s`}
              </CardDescription>
            </CardHeader>
            <CardContent className="px-0 sm:px-0">
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Time</TableHead>
                      <TableHead>Latency</TableHead>
                      <TableHead>Op</TableHead>
                      <TableHead>Bucket</TableHead>
                      <TableHead>Object key</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>Principal</TableHead>
                      <TableHead>Source IP</TableHead>
                      <TableHead>Request ID</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {showSkeleton &&
                      Array.from({ length: 6 }).map((_, i) => (
                        <TableRow key={`sk-${i}`}>
                          <TableCell colSpan={9} className="py-3">
                            <Skeleton className="h-5 w-full" />
                          </TableCell>
                        </TableRow>
                      ))}
                    {!showSkeleton && rows.length === 0 && (
                      <TableRow>
                        <TableCell colSpan={9} className="py-10 text-center">
                          <div className="space-y-2">
                            <div className="text-sm font-medium">No slow queries</div>
                            <div className="text-xs text-muted-foreground">
                              Nothing above {minMs} ms in the last {rangeKey}.
                            </div>
                          </div>
                        </TableCell>
                      </TableRow>
                    )}
                    {rows.map((r) => (
                      <TableRow key={r.request_id || `${r.ts}-${r.op}`}>
                        <TableCell title={r.ts}>
                          {new Date(r.ts).toLocaleString()}
                        </TableCell>
                        <TableCell>
                          <span
                            className={cn(
                              'inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-semibold tabular-nums',
                              latencyBadgeClass(r.latency_ms),
                            )}
                          >
                            {r.latency_ms} ms
                          </span>
                        </TableCell>
                        <TableCell className="font-mono text-xs">{r.op}</TableCell>
                        <TableCell className="font-mono text-xs">
                          {r.bucket || '—'}
                        </TableCell>
                        <TableCell
                          className="max-w-[16rem] truncate font-mono text-xs"
                          title={r.object_key}
                        >
                          {r.object_key || '—'}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {r.status || '—'}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {r.principal || '—'}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {r.source_ip || '—'}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {r.request_id ? (
                            <Link
                              to={`/diagnostics/trace/${encodeURIComponent(r.request_id)}`}
                              title="Trace"
                              className="inline-flex items-center gap-1 text-primary underline-offset-2 hover:underline"
                            >
                              {shortenID(r.request_id, 6, 4)}
                              <ExternalLink className="h-3 w-3" aria-hidden />
                            </Link>
                          ) : (
                            '—'
                          )}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <div className="flex flex-col items-center justify-between gap-2 border-t px-4 py-3 text-sm text-muted-foreground sm:flex-row sm:px-6">
                <div>
                  Page {pageNumber}
                  {pageNumber > 1 && (
                    <button
                      type="button"
                      onClick={handleResetPaging}
                      className="ml-2 underline-offset-2 hover:underline"
                    >
                      back to first
                    </button>
                  )}
                </div>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={handleLoadMore}
                  disabled={!nextToken || q.isFetching}
                >
                  Load more
                </Button>
              </div>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="histogram" className="mt-4">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Latency histogram</CardTitle>
              <CardDescription>
                Log-scale count of requests in each latency bucket on the
                current filtered page.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {showSkeleton ? (
                <Skeleton className="h-72 w-full" />
              ) : rows.length === 0 ? (
                <div className="flex h-72 items-center justify-center text-sm text-muted-foreground">
                  No rows in the current window.
                </div>
              ) : (
                <div className="h-72 w-full" aria-label="Latency histogram chart">
                  <ResponsiveContainer width="100%" height="100%">
                    <BarChart
                      data={histogram}
                      margin={{ top: 8, right: 16, left: 0, bottom: 0 }}
                    >
                      <CartesianGrid strokeDasharray="3 3" className="stroke-border/40" />
                      <XAxis
                        dataKey="bucket"
                        stroke="currentColor"
                        className="text-xs text-muted-foreground"
                      />
                      <YAxis
                        scale="log"
                        domain={[0.5, 'auto']}
                        allowDataOverflow
                        tickFormatter={(v: number) => String(Math.round(v))}
                        stroke="currentColor"
                        className="text-xs text-muted-foreground"
                      />
                      <Tooltip
                        formatter={(v: number) => [`${v}`, 'count']}
                      />
                      <Bar
                        dataKey="count"
                        fill="hsl(220 90% 56%)"
                        isAnimationActive={false}
                      />
                    </BarChart>
                  </ResponsiveContainer>
                </div>
              )}
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>
    </div>
  );
}
