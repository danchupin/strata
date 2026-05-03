import { useState, useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertTriangle } from 'lucide-react';
import {
  Area,
  AreaChart,
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';

import {
  fetchMetricsTimeseries,
  type MetricsTimeseriesResponse,
} from '@/api/client';
import { queryKeys } from '@/lib/query';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';

// rangeOptions drives the segmented control. The polling cadence + default
// step are picked so a long range doesn't refetch as aggressively as a short
// one — a 7d window changing once every 5 min is plenty.
type RangeKey = '15m' | '1h' | '6h' | '24h' | '7d';

interface RangeOption {
  key: RangeKey;
  label: string;
  refetchInterval: number; // ms
  step: string; // duration string passed to /metrics/timeseries
}

const rangeOptions: RangeOption[] = [
  { key: '15m', label: '15m', refetchInterval: 5_000, step: '30s' },
  { key: '1h', label: '1h', refetchInterval: 5_000, step: '1m' },
  { key: '6h', label: '6h', refetchInterval: 5_000, step: '5m' },
  { key: '24h', label: '24h', refetchInterval: 30_000, step: '15m' },
  { key: '7d', label: '7d', refetchInterval: 300_000, step: '1h' },
];

function findRange(key: RangeKey): RangeOption {
  return rangeOptions.find((r) => r.key === key) ?? rangeOptions[0];
}

// useTimeseries wraps the metrics endpoint with TanStack Query and a per-range
// refetchInterval override. queryKey includes (metric, range, step) so range
// changes invalidate cleanly.
function useTimeseries(metric: string, range: RangeOption) {
  return useQuery({
    queryKey: queryKeys.metrics.timeseries(metric, range.key, range.step),
    queryFn: () => fetchMetricsTimeseries({ metric, range: range.key, step: range.step }),
    refetchInterval: range.refetchInterval,
    staleTime: range.refetchInterval / 2,
    meta: { label: `metrics ${metric}` },
  });
}

// ChartPoint is the recharts row shape — one row per timestamp with one key
// per series. Latency layers p50/p95/p99 onto the same row by ts; bytes does
// the same with bytes_in / bytes_out.
type ChartPoint = { ts: number } & Record<string, number>;

function mergePoints(
  responses: Array<{ key: string; resp: MetricsTimeseriesResponse | undefined }>,
): ChartPoint[] {
  const byTs = new Map<number, ChartPoint>();
  for (const { key, resp } of responses) {
    if (!resp) continue;
    for (const series of resp.series) {
      // The Go side names the series after the metric (or "p50/p95/p99" for
      // latency); we override with the caller's `key` so the chart legend
      // matches the prop name we gave it.
      for (const [ts, value] of series.points) {
        let row = byTs.get(ts);
        if (!row) {
          row = { ts } as ChartPoint;
          byTs.set(ts, row);
        }
        if (Number.isFinite(value)) {
          row[key] = value;
        }
      }
    }
  }
  return Array.from(byTs.values()).sort((a, b) => a.ts - b.ts);
}

function formatTick(ts: number, range: RangeKey): string {
  const d = new Date(ts);
  if (range === '7d') {
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  }
  if (range === '24h' || range === '6h') {
    return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  }
  return d.toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

function formatTooltipTs(ts: number): string {
  return new Date(ts).toLocaleString();
}

function formatNumber(n: number, digits = 2): string {
  if (!Number.isFinite(n)) return '—';
  if (Math.abs(n) >= 1000) return n.toFixed(0);
  return n.toFixed(digits);
}

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B/s';
  const units = ['B/s', 'KiB/s', 'MiB/s', 'GiB/s', 'TiB/s'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(2)} ${units[i]}`;
}

function formatPercent(n: number): string {
  if (!Number.isFinite(n)) return '—';
  return `${(n * 100).toFixed(2)}%`;
}

function formatSeconds(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (n >= 1) return `${n.toFixed(2)}s`;
  if (n >= 0.001) return `${(n * 1000).toFixed(1)}ms`;
  return `${(n * 1_000_000).toFixed(0)}µs`;
}

interface ChartCardProps {
  title: string;
  description: string;
  metricsAvailable: boolean;
  loading: boolean;
  data: ChartPoint[];
  range: RangeKey;
  children: (data: ChartPoint[]) => React.ReactNode;
}

function MetricsUnavailable({ inline }: { inline?: boolean }) {
  return (
    <div
      className={cn(
        'flex items-start gap-2 text-sm text-amber-900 dark:text-amber-200',
        inline ? '' : 'rounded-md border border-amber-500/40 bg-amber-500/5 p-3',
      )}
    >
      <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
      <div>
        Metrics unavailable — set <code className="font-mono">STRATA_PROMETHEUS_URL</code>.
      </div>
    </div>
  );
}

function ChartCard({
  title,
  description,
  metricsAvailable,
  loading,
  data,
  children,
}: ChartCardProps) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{title}</CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent>
        {!metricsAvailable && !loading && <MetricsUnavailable />}
        {loading && data.length === 0 && (
          <Skeleton className="h-56 w-full" />
        )}
        {data.length > 0 && (
          <div className="h-56 w-full" aria-label={`${title} chart`}>
            <ResponsiveContainer width="100%" height="100%">
              {children(data) as React.ReactElement}
            </ResponsiveContainer>
          </div>
        )}
        {metricsAvailable && !loading && data.length === 0 && (
          <div className="flex h-56 items-center justify-center text-sm text-muted-foreground">
            No data in this window.
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function RequestRateChart({ range }: { range: RangeOption }) {
  const q = useTimeseries('request_rate', range);
  const metricsAvailable = q.data?.metrics_available ?? false;
  const data = useMemo(
    () => mergePoints([{ key: 'rate', resp: q.data }]),
    [q.data],
  );
  return (
    <ChartCard
      title="Request rate"
      description="HTTP requests per second across the cluster."
      metricsAvailable={metricsAvailable}
      loading={q.isPending}
      data={data}
      range={range.key}
    >
      {(d) => (
        <AreaChart data={d} margin={{ top: 4, right: 16, left: 0, bottom: 0 }}>
          <defs>
            <linearGradient id="rateGrad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="hsl(220 90% 56%)" stopOpacity={0.4} />
              <stop offset="100%" stopColor="hsl(220 90% 56%)" stopOpacity={0} />
            </linearGradient>
          </defs>
          <CartesianGrid strokeDasharray="3 3" className="stroke-border/40" />
          <XAxis
            dataKey="ts"
            type="number"
            scale="time"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(t) => formatTick(t, range.key)}
            stroke="currentColor"
            className="text-xs text-muted-foreground"
          />
          <YAxis
            tickFormatter={(v) => formatNumber(v)}
            stroke="currentColor"
            className="text-xs text-muted-foreground"
          />
          <Tooltip
            labelFormatter={(t) => formatTooltipTs(Number(t))}
            formatter={(v: number) => [`${formatNumber(v)} req/s`, 'rate']}
          />
          <Legend />
          <Area
            type="monotone"
            dataKey="rate"
            name="req/s"
            stroke="hsl(220 90% 56%)"
            fill="url(#rateGrad)"
            strokeWidth={2}
            isAnimationActive={false}
          />
        </AreaChart>
      )}
    </ChartCard>
  );
}

function LatencyChart({ range }: { range: RangeOption }) {
  const p50 = useTimeseries('latency_p50', range);
  const p95 = useTimeseries('latency_p95', range);
  const p99 = useTimeseries('latency_p99', range);
  const metricsAvailable =
    (p50.data?.metrics_available ?? false) ||
    (p95.data?.metrics_available ?? false) ||
    (p99.data?.metrics_available ?? false);
  const data = useMemo(
    () =>
      mergePoints([
        { key: 'p50', resp: p50.data },
        { key: 'p95', resp: p95.data },
        { key: 'p99', resp: p99.data },
      ]),
    [p50.data, p95.data, p99.data],
  );
  const loading = p50.isPending || p95.isPending || p99.isPending;
  return (
    <ChartCard
      title="Latency"
      description="Request latency percentiles (seconds)."
      metricsAvailable={metricsAvailable}
      loading={loading}
      data={data}
      range={range.key}
    >
      {(d) => (
        <LineChart data={d} margin={{ top: 4, right: 16, left: 0, bottom: 0 }}>
          <CartesianGrid strokeDasharray="3 3" className="stroke-border/40" />
          <XAxis
            dataKey="ts"
            type="number"
            scale="time"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(t) => formatTick(t, range.key)}
            stroke="currentColor"
            className="text-xs text-muted-foreground"
          />
          <YAxis
            tickFormatter={(v) => formatSeconds(v)}
            stroke="currentColor"
            className="text-xs text-muted-foreground"
          />
          <Tooltip
            labelFormatter={(t) => formatTooltipTs(Number(t))}
            formatter={(v: number, name) => [formatSeconds(v), name]}
          />
          <Legend />
          <Line
            type="monotone"
            dataKey="p50"
            stroke="hsl(142 71% 40%)"
            strokeWidth={2}
            dot={false}
            isAnimationActive={false}
            connectNulls
          />
          <Line
            type="monotone"
            dataKey="p95"
            stroke="hsl(38 92% 50%)"
            strokeWidth={2}
            dot={false}
            isAnimationActive={false}
            connectNulls
          />
          <Line
            type="monotone"
            dataKey="p99"
            stroke="hsl(0 72% 50%)"
            strokeWidth={2}
            dot={false}
            isAnimationActive={false}
            connectNulls
          />
        </LineChart>
      )}
    </ChartCard>
  );
}

function ErrorRateChart({ range }: { range: RangeOption }) {
  const q = useTimeseries('error_rate', range);
  const metricsAvailable = q.data?.metrics_available ?? false;
  const data = useMemo(
    () => mergePoints([{ key: 'error_rate', resp: q.data }]),
    [q.data],
  );
  return (
    <ChartCard
      title="Error rate"
      description="Share of HTTP requests returning a 5xx response."
      metricsAvailable={metricsAvailable}
      loading={q.isPending}
      data={data}
      range={range.key}
    >
      {(d) => (
        <AreaChart data={d} margin={{ top: 4, right: 16, left: 0, bottom: 0 }}>
          <defs>
            <linearGradient id="errGrad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="hsl(0 72% 50%)" stopOpacity={0.4} />
              <stop offset="100%" stopColor="hsl(0 72% 50%)" stopOpacity={0} />
            </linearGradient>
          </defs>
          <CartesianGrid strokeDasharray="3 3" className="stroke-border/40" />
          <XAxis
            dataKey="ts"
            type="number"
            scale="time"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(t) => formatTick(t, range.key)}
            stroke="currentColor"
            className="text-xs text-muted-foreground"
          />
          <YAxis
            tickFormatter={(v) => formatPercent(v)}
            stroke="currentColor"
            className="text-xs text-muted-foreground"
            domain={[0, 'auto']}
          />
          <Tooltip
            labelFormatter={(t) => formatTooltipTs(Number(t))}
            formatter={(v: number) => [formatPercent(v), '5xx share']}
          />
          <Legend />
          <Area
            type="monotone"
            dataKey="error_rate"
            name="5xx share"
            stroke="hsl(0 72% 50%)"
            fill="url(#errGrad)"
            strokeWidth={2}
            isAnimationActive={false}
          />
        </AreaChart>
      )}
    </ChartCard>
  );
}

function BytesChart({ range }: { range: RangeOption }) {
  const inQ = useTimeseries('bytes_in', range);
  const outQ = useTimeseries('bytes_out', range);
  const metricsAvailable =
    (inQ.data?.metrics_available ?? false) ||
    (outQ.data?.metrics_available ?? false);
  const data = useMemo(
    () =>
      mergePoints([
        { key: 'bytes_in', resp: inQ.data },
        { key: 'bytes_out', resp: outQ.data },
      ]),
    [inQ.data, outQ.data],
  );
  const loading = inQ.isPending || outQ.isPending;
  return (
    <ChartCard
      title="Bytes"
      description="Network throughput in / out (per second)."
      metricsAvailable={metricsAvailable}
      loading={loading}
      data={data}
      range={range.key}
    >
      {(d) => (
        <LineChart data={d} margin={{ top: 4, right: 16, left: 0, bottom: 0 }}>
          <CartesianGrid strokeDasharray="3 3" className="stroke-border/40" />
          <XAxis
            dataKey="ts"
            type="number"
            scale="time"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(t) => formatTick(t, range.key)}
            stroke="currentColor"
            className="text-xs text-muted-foreground"
          />
          <YAxis
            tickFormatter={(v) => formatBytes(v)}
            stroke="currentColor"
            className="text-xs text-muted-foreground"
          />
          <Tooltip
            labelFormatter={(t) => formatTooltipTs(Number(t))}
            formatter={(v: number, name) => [formatBytes(v), name]}
          />
          <Legend />
          <Line
            type="monotone"
            dataKey="bytes_in"
            name="in"
            stroke="hsl(220 90% 56%)"
            strokeWidth={2}
            dot={false}
            isAnimationActive={false}
            connectNulls
          />
          <Line
            type="monotone"
            dataKey="bytes_out"
            name="out"
            stroke="hsl(280 80% 55%)"
            strokeWidth={2}
            dot={false}
            isAnimationActive={false}
            connectNulls
          />
        </LineChart>
      )}
    </ChartCard>
  );
}

function RangePicker({
  value,
  onChange,
}: {
  value: RangeKey;
  onChange: (next: RangeKey) => void;
}) {
  return (
    <div
      role="group"
      aria-label="Time range"
      className="inline-flex rounded-md border border-input bg-background p-0.5 text-sm"
    >
      {rangeOptions.map((opt) => (
        <button
          key={opt.key}
          type="button"
          onClick={() => onChange(opt.key)}
          aria-pressed={value === opt.key}
          className={cn(
            'rounded-sm px-3 py-1 font-medium transition-colors',
            value === opt.key
              ? 'bg-primary text-primary-foreground shadow-sm'
              : 'text-muted-foreground hover:text-foreground',
          )}
        >
          {opt.label}
        </button>
      ))}
    </div>
  );
}

export function MetricsPage() {
  const [rangeKey, setRangeKey] = useState<RangeKey>('1h');
  const range = useMemo(() => findRange(rangeKey), [rangeKey]);

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Metrics</h1>
          <p className="text-sm text-muted-foreground">
            Request rate, latency, error rate, and bytes over a selectable
            window.
          </p>
        </div>
        <RangePicker value={rangeKey} onChange={setRangeKey} />
      </div>

      <div className="grid gap-6 xl:grid-cols-2">
        <RequestRateChart range={range} />
        <LatencyChart range={range} />
        <ErrorRateChart range={range} />
        <BytesChart range={range} />
      </div>
    </div>
  );
}
