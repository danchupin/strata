import { useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { ExternalLink, Info, RefreshCw } from 'lucide-react';
import {
  CartesianGrid,
  Line,
  LineChart,
  ReferenceArea,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';

import {
  fetchBucketReplicationLag,
  type AdminApiError,
  type BucketDetail,
  type BucketReplicationLagPoint,
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
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';

const POLL_INTERVAL_MS = 30_000;
// Threshold lines + danger band match the AC literally — 1s, 60s, 600s, with
// the band starting at 600s so anything older than 10 minutes pops red.
const THRESHOLD_LINES_SECONDS = [1, 60, 600] as const;
const DANGER_BAND_SECONDS = 600;
const DANGER_BAND_TOP = 1e9;

type RangeKey = '15m' | '1h' | '6h' | '24h';
interface RangeOption {
  key: RangeKey;
  label: string;
  range: string;
}

const RANGE_OPTIONS: RangeOption[] = [
  { key: '15m', label: '15m', range: '15m' },
  { key: '1h', label: '1h', range: '1h' },
  { key: '6h', label: '6h', range: '6h' },
  { key: '24h', label: '24h', range: '24h' },
];

function isMetricsUnavailable(err: unknown): boolean {
  return (
    err != null &&
    typeof err === 'object' &&
    (err as AdminApiError).code === 'MetricsUnavailable'
  );
}

interface ChartRow {
  ts: number; // epoch ms
  value: number;
}

function pointsToRows(points: BucketReplicationLagPoint[]): ChartRow[] {
  return points
    .map((p) => ({ ts: Date.parse(p.ts), value: Number(p.value) }))
    .filter((r) => Number.isFinite(r.ts) && Number.isFinite(r.value));
}

function formatSeconds(v: number): string {
  if (!Number.isFinite(v) || v < 0) return '—';
  if (v < 1) return `${(v * 1000).toFixed(0)} ms`;
  if (v < 60) return `${v.toFixed(1)} s`;
  if (v < 3600) return `${(v / 60).toFixed(1)} m`;
  return `${(v / 3600).toFixed(2)} h`;
}

function formatClock(epochMs: number): string {
  if (!Number.isFinite(epochMs)) return '';
  return new Date(epochMs).toLocaleTimeString();
}

interface Props {
  bucket: BucketDetail;
}

export function BucketReplicationLagTab({ bucket }: Props) {
  const [rangeKey, setRangeKey] = useState<RangeKey>('1h');
  const opt = useMemo(
    () => RANGE_OPTIONS.find((r) => r.key === rangeKey) ?? RANGE_OPTIONS[1],
    [rangeKey],
  );

  const q = useQuery({
    queryKey: queryKeys.buckets.replicationLag(bucket.name, opt.range),
    queryFn: () => fetchBucketReplicationLag(bucket.name, opt.range),
    refetchInterval: POLL_INTERVAL_MS,
    placeholderData: keepPreviousData,
    retry: (failureCount, err) =>
      !isMetricsUnavailable(err) && failureCount < 1,
    meta: { label: 'replication lag' },
  });

  const rows = useMemo<ChartRow[]>(
    () => pointsToRows(q.data?.values ?? []),
    [q.data?.values],
  );

  const showSkeleton = q.isPending && !q.data;
  const metricsUnavailable = isMetricsUnavailable(q.error);
  const errorMessage =
    !q.data && !metricsUnavailable && q.error instanceof Error
      ? q.error.message
      : null;

  function handleRefresh() {
    void queryClient.invalidateQueries({
      queryKey: queryKeys.buckets.replicationLag(bucket.name, opt.range),
    });
  }

  if (q.data?.empty) {
    return (
      <Card>
        <CardContent className="flex items-start gap-3 py-6 text-sm text-muted-foreground">
          <Info className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
          <div>
            <div className="font-medium text-foreground">
              No replication configured
            </div>
            <p>
              {q.data.reason ||
                'This bucket has no replication configuration. Set one via PutBucketReplication on the S3 surface to enable cross-cluster replication.'}
            </p>
          </div>
        </CardContent>
      </Card>
    );
  }

  if (metricsUnavailable) {
    return (
      <Card className="border-amber-500/40 bg-amber-500/5">
        <CardContent className="flex items-start gap-3 py-6 text-sm text-amber-900 dark:text-amber-200">
          <Info className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
          <div className="space-y-1">
            <div className="font-medium">Metrics unavailable</div>
            <p>
              Replication lag is sourced from Prometheus. Set{' '}
              <code>STRATA_PROMETHEUS_URL</code> on the gateway and a Prometheus
              instance scraping <code>strata_replication_queue_age_seconds</code>{' '}
              to populate this chart.
            </p>
            <a
              href="https://danchupin.github.io/strata/best-practices/monitoring/#prometheus"
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-amber-900 underline-offset-2 hover:underline dark:text-amber-100"
            >
              Monitoring guide — Prometheus setup
              <ExternalLink className="h-3 w-3" aria-hidden />
            </a>
          </div>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-4">
      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="py-4 text-sm text-destructive">
            <div className="font-medium">Failed to load replication lag</div>
            <div className="text-xs text-destructive/80">{errorMessage}</div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
          <div className="space-y-1">
            <CardTitle className="text-base">Replication queue age</CardTitle>
            <CardDescription>
              Oldest pending replication_queue row for this bucket. Threshold
              dashed lines at 1 s / 60 s / 600 s; the red band marks lag above
              10 minutes — replicator is falling behind.
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <div className="inline-flex items-center rounded-md border border-input bg-background p-0.5">
              {RANGE_OPTIONS.map((r) => (
                <button
                  key={r.key}
                  type="button"
                  className={cn(
                    'px-2.5 py-1 text-xs font-medium rounded-sm transition-colors',
                    r.key === rangeKey
                      ? 'bg-secondary text-foreground'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                  aria-pressed={r.key === rangeKey}
                  onClick={() => setRangeKey(r.key)}
                >
                  {r.label}
                </button>
              ))}
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching}
              aria-label="Refresh replication lag"
            >
              <RefreshCw
                className={cn(
                  'mr-1.5 h-3.5 w-3.5',
                  q.isFetching && 'animate-spin',
                )}
                aria-hidden
              />
              Refresh
            </Button>
          </div>
        </CardHeader>
        <CardContent>
          {showSkeleton ? (
            <Skeleton className="h-72 w-full" />
          ) : rows.length === 0 ? (
            <div className="flex h-72 items-center justify-center rounded-md border border-dashed border-border/60 bg-muted/30 text-sm text-muted-foreground">
              No replication lag samples in the selected window.
            </div>
          ) : (
            <div
              className="h-72 w-full"
              aria-label={`Replication lag for ${bucket.name} over ${opt.label}`}
            >
              <ResponsiveContainer width="100%" height="100%">
                <LineChart
                  data={rows}
                  margin={{ top: 8, right: 16, left: 0, bottom: 0 }}
                >
                  <CartesianGrid
                    strokeDasharray="3 3"
                    className="stroke-border/40"
                  />
                  <XAxis
                    dataKey="ts"
                    type="number"
                    domain={['dataMin', 'dataMax']}
                    tickFormatter={formatClock}
                    stroke="currentColor"
                    className="text-xs text-muted-foreground"
                  />
                  <YAxis
                    tickFormatter={(v) => formatSeconds(Number(v))}
                    stroke="currentColor"
                    className="text-xs text-muted-foreground"
                    width={64}
                  />
                  <ReferenceArea
                    y1={DANGER_BAND_SECONDS}
                    y2={DANGER_BAND_TOP}
                    fill="hsl(0 84% 60%)"
                    fillOpacity={0.08}
                    stroke="none"
                    ifOverflow="extendDomain"
                  />
                  {THRESHOLD_LINES_SECONDS.map((s) => (
                    <ReferenceLine
                      key={s}
                      y={s}
                      stroke="hsl(0 0% 60%)"
                      strokeDasharray="4 4"
                      label={{
                        value: formatSeconds(s),
                        position: 'right',
                        fill: 'currentColor',
                        fontSize: 11,
                      }}
                    />
                  ))}
                  <Tooltip
                    formatter={(v: number) => [formatSeconds(v), 'lag']}
                    labelFormatter={(label) => formatClock(Number(label))}
                  />
                  <Line
                    type="monotone"
                    dataKey="value"
                    stroke="hsl(220 90% 56%)"
                    strokeWidth={2}
                    dot={false}
                    isAnimationActive={false}
                  />
                </LineChart>
              </ResponsiveContainer>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

export default BucketReplicationLagTab;
