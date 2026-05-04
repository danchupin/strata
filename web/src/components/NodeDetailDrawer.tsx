import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, Info } from 'lucide-react';
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';

import {
  fetchNodeDrilldown,
  type AdminApiError,
  type ClusterNode,
  type NodeMetricPoint,
} from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Card, CardContent } from '@/components/ui/card';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
import { Skeleton } from '@/components/ui/skeleton';
import { queryKeys } from '@/lib/query';
import { cn } from '@/lib/utils';

// US-011 — per-node drilldown drawer. Renders the heartbeat row header plus
// 5 stacked recharts LineChart sparklines (CPU / memory / open-FDs /
// goroutines / GC pause). The endpoint is gated on Prometheus availability;
// on TiKV-backed clusters we surface an explainer because the heartbeat
// table only carries the local replica until the dedicated heartbeat
// backend lands.

type RangeKey = '15m' | '1h' | '6h';

interface RangeOption {
  key: RangeKey;
  label: string;
}

const RANGE_OPTIONS: RangeOption[] = [
  { key: '15m', label: '15 min' },
  { key: '1h', label: '1 hour' },
  { key: '6h', label: '6 hours' },
];

const POLL_INTERVAL_MS = 30_000;

interface NodeDetailDrawerProps {
  node: ClusterNode | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  metaBackend?: string;
}

function isMetricsUnavailable(err: unknown): boolean {
  return (err as AdminApiError | null)?.code === 'MetricsUnavailable';
}

function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—';
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

function formatBytes(value: number): string {
  if (!Number.isFinite(value)) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = value;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)} ${units[i]}`;
}

function formatTick(ts: string): string {
  const d = new Date(ts);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

interface SparklineProps {
  title: string;
  unit: string;
  data: NodeMetricPoint[];
  color: string;
  format?: (value: number) => string;
}

function Sparkline({ title, unit, data, color, format }: SparklineProps) {
  const rows = useMemo(
    () =>
      data.map((p) => ({
        ts: p.ts,
        v: Number.isFinite(p.value) ? p.value : 0,
      })),
    [data],
  );
  const last = rows.length > 0 ? rows[rows.length - 1].v : null;
  const lastLabel =
    last == null ? '—' : format ? format(last) : last.toFixed(2);
  return (
    <Card>
      <CardContent className="space-y-1 px-4 py-3">
        <div className="flex items-baseline justify-between">
          <div className="text-xs uppercase tracking-wide text-muted-foreground">
            {title}
          </div>
          <div className="font-mono text-sm">
            {lastLabel}
            <span className="ml-1 text-xs text-muted-foreground">{unit}</span>
          </div>
        </div>
        <div className="h-24">
          {rows.length === 0 ? (
            <div className="flex h-full items-center justify-center text-xs text-muted-foreground">
              No data points in range
            </div>
          ) : (
            <ResponsiveContainer width="100%" height="100%">
              <LineChart data={rows} margin={{ top: 4, right: 4, left: 0, bottom: 0 }}>
                <CartesianGrid strokeDasharray="3 3" stroke="currentColor" opacity={0.1} />
                <XAxis
                  dataKey="ts"
                  tickFormatter={formatTick}
                  tick={{ fontSize: 10 }}
                  minTickGap={24}
                  stroke="currentColor"
                  opacity={0.5}
                />
                <YAxis
                  tick={{ fontSize: 10 }}
                  tickFormatter={(v: number) =>
                    format ? format(v) : v.toFixed(v < 1 ? 2 : 0)
                  }
                  width={56}
                  stroke="currentColor"
                  opacity={0.5}
                />
                <Tooltip
                  contentStyle={{ fontSize: 12 }}
                  labelFormatter={(ts: string) => new Date(ts).toLocaleString()}
                  formatter={(v: number) =>
                    format ? format(v) : v.toFixed(2)
                  }
                />
                <Line
                  type="monotone"
                  dataKey="v"
                  stroke={color}
                  dot={false}
                  strokeWidth={1.5}
                  isAnimationActive={false}
                />
              </LineChart>
            </ResponsiveContainer>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

function ChipList({ items, empty }: { items: string[]; empty: string }) {
  if (!items || items.length === 0) {
    return <span className="text-xs text-muted-foreground">{empty}</span>;
  }
  return (
    <div className="flex flex-wrap gap-1">
      {items.map((it) => (
        <Badge key={it} variant="secondary" className="font-normal">
          {it}
        </Badge>
      ))}
    </div>
  );
}

export function NodeDetailDrawer({
  node,
  open,
  onOpenChange,
  metaBackend,
}: NodeDetailDrawerProps) {
  const [range, setRange] = useState<RangeKey>('15m');
  const nodeID = node?.id ?? '';
  const enabled = open && nodeID !== '';
  const q = useQuery({
    queryKey: queryKeys.diagnostics.node(nodeID, range),
    queryFn: () => fetchNodeDrilldown(nodeID, range),
    enabled,
    refetchInterval: enabled ? POLL_INTERVAL_MS : false,
    staleTime: POLL_INTERVAL_MS / 2,
    retry: (count, err) => !isMetricsUnavailable(err) && count < 1,
    meta: { label: `node ${nodeID}`, silent: true },
  });

  const headerNode = q.data?.node ?? node;
  const tikvLocalOnly = (metaBackend ?? '').toLowerCase() === 'tikv';

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className={cn('w-full overflow-y-auto sm:max-w-2xl')}
        aria-describedby="node-detail-desc"
      >
        <SheetHeader>
          <SheetTitle className="font-mono text-base">
            {headerNode?.id ?? '—'}
          </SheetTitle>
          <SheetDescription id="node-detail-desc">
            Per-node CPU / memory / FD / goroutine / GC sparklines. Click outside
            or press Esc to close.
          </SheetDescription>
        </SheetHeader>

        <div className="mt-4 grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
          <div>
            <div className="text-xs uppercase tracking-wide text-muted-foreground">
              Address
            </div>
            <div className="mt-1 font-mono text-xs break-all">
              {headerNode?.address || '—'}
            </div>
          </div>
          <div>
            <div className="text-xs uppercase tracking-wide text-muted-foreground">
              Version
            </div>
            <div className="mt-1 font-mono text-xs">
              {headerNode?.version || '—'}
            </div>
          </div>
          <div>
            <div className="text-xs uppercase tracking-wide text-muted-foreground">
              Uptime
            </div>
            <div className="mt-1">{formatUptime(headerNode?.uptime_sec ?? 0)}</div>
          </div>
          <div>
            <div className="text-xs uppercase tracking-wide text-muted-foreground">
              Status
            </div>
            <div className="mt-1 capitalize">{headerNode?.status || '—'}</div>
          </div>
          <div className="col-span-2">
            <div className="text-xs uppercase tracking-wide text-muted-foreground">
              Workers
            </div>
            <div className="mt-1">
              <ChipList items={headerNode?.workers ?? []} empty="none" />
            </div>
          </div>
          <div className="col-span-2">
            <div className="text-xs uppercase tracking-wide text-muted-foreground">
              Leader for
            </div>
            <div className="mt-1">
              <ChipList items={headerNode?.leader_for ?? []} empty="none" />
            </div>
          </div>
        </div>

        <div className="mt-5 flex items-center justify-between">
          <div className="inline-flex rounded-md border border-border p-0.5">
            {RANGE_OPTIONS.map((r) => (
              <button
                key={r.key}
                type="button"
                className={cn(
                  'px-2.5 py-1 text-xs font-medium rounded-sm transition-colors',
                  range === r.key
                    ? 'bg-primary text-primary-foreground'
                    : 'text-muted-foreground hover:bg-muted',
                )}
                onClick={() => setRange(r.key)}
              >
                {r.label}
              </button>
            ))}
          </div>
          <span className="text-xs text-muted-foreground">
            Polls every {Math.round(POLL_INTERVAL_MS / 1000)}s
          </span>
        </div>

        {tikvLocalOnly && (
          <Card className="mt-4 border-blue-500/40 bg-blue-500/5">
            <CardContent className="flex items-start gap-2 py-3 text-sm text-blue-900 dark:text-blue-200">
              <Info className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                TiKV-backed clusters surface only the local replica today. The
                full multi-node experience lands with the dedicated TiKV
                heartbeat backend (ROADMAP — Web UI / TiKV heartbeat).
                Cassandra-backed clusters get cluster-wide drilldown.
              </div>
            </CardContent>
          </Card>
        )}

        <div className="mt-4 space-y-3">
          {q.isPending && enabled && !q.data && (
            <>
              <Skeleton className="h-32 w-full" />
              <Skeleton className="h-32 w-full" />
              <Skeleton className="h-32 w-full" />
            </>
          )}
          {!q.isPending && q.error && isMetricsUnavailable(q.error) && (
            <Card className="border-amber-500/40 bg-amber-500/5">
              <CardContent className="flex items-start gap-2 py-3 text-sm text-amber-900 dark:text-amber-200">
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                <div>
                  <div className="font-medium">Prometheus not configured</div>
                  <div className="mt-1 text-xs">
                    Set <code className="font-mono">STRATA_PROMETHEUS_URL</code>{' '}
                    so the gateway can render per-node sparklines. See{' '}
                    <a className="underline" href="/docs/ui.md#prometheus-setup">
                      docs/ui.md#prometheus-setup
                    </a>
                    .
                  </div>
                </div>
              </CardContent>
            </Card>
          )}
          {!q.isPending && q.error && !isMetricsUnavailable(q.error) && (
            <Card className="border-destructive/40 bg-destructive/5">
              <CardContent className="flex items-start gap-2 py-3 text-sm text-destructive">
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                <div>
                  {q.error instanceof Error
                    ? q.error.message
                    : 'failed to load node drilldown'}
                </div>
              </CardContent>
            </Card>
          )}
          {q.data && (
            <>
              <Sparkline
                title="CPU"
                unit="cores"
                data={q.data.cpu}
                color="hsl(220 90% 56%)"
                format={(v) => v.toFixed(2)}
              />
              <Sparkline
                title="Resident memory"
                unit=""
                data={q.data.mem}
                color="hsl(160 70% 40%)"
                format={formatBytes}
              />
              <Sparkline
                title="Open file descriptors"
                unit="fds"
                data={q.data.fds}
                color="hsl(35 90% 50%)"
              />
              <Sparkline
                title="Goroutines"
                unit=""
                data={q.data.goroutines}
                color="hsl(280 70% 50%)"
              />
              <Sparkline
                title="GC pause (p99)"
                unit="s"
                data={q.data.gc_pause}
                color="hsl(0 70% 50%)"
                format={(v) => v.toFixed(4)}
              />
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
