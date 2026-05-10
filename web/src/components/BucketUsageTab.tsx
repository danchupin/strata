import { useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, Pencil, RefreshCw, Trash2 } from 'lucide-react';
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
  deleteBucketQuota,
  fetchBucketQuota,
  fetchBucketUsage,
  type BucketDetail,
  type UsageRow,
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { EditBucketQuotaDialog } from '@/components/EditBucketQuotaDialog';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

interface Props {
  bucket: BucketDetail;
}

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function formatCount(n: number): string {
  if (!Number.isFinite(n)) return '0';
  return n.toLocaleString();
}

// usageDayWindow returns (startISO, endISO) covering the last 30 days
// inclusive, in UTC. Caller passes both to /admin/v1/buckets/{name}/usage.
function usageDayWindow(): { start: string; end: string } {
  const now = new Date();
  const endUTC = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const startUTC = new Date(endUTC.getTime() - 29 * 86400 * 1000);
  const fmt = (d: Date) => d.toISOString().slice(0, 10);
  return { start: fmt(startUTC), end: fmt(endUTC) };
}

export function BucketUsageTab({ bucket }: Props) {
  const [editOpen, setEditOpen] = useState(false);
  const range = useMemo(usageDayWindow, []);

  const quotaQ = useQuery({
    queryKey: queryKeys.buckets.quota(bucket.name),
    queryFn: () => fetchBucketQuota(bucket.name),
    meta: { label: 'bucket quota', silent: true },
    retry: false,
  });

  const usageQ = useQuery({
    queryKey: queryKeys.buckets.usage(bucket.name, range.start, range.end),
    queryFn: () => fetchBucketUsage(bucket.name, range.start, range.end),
    placeholderData: keepPreviousData,
    meta: { label: 'bucket usage history' },
  });

  const quota = quotaQ.data ?? null;
  const rows: UsageRow[] = usageQ.data?.rows ?? [];

  const usedBytes = bucket.size_bytes ?? 0;
  const usedObjects = bucket.object_count ?? 0;
  const maxBytes = quota?.max_bytes ?? 0;
  const fillPct = maxBytes > 0 ? Math.min(100, (usedBytes / maxBytes) * 100) : 0;

  // Per-day average bytes = sum(byte_seconds) / 86400 across all classes for
  // the day. The chart series renders one point per day in the window so a
  // gap (no rollup row yet for that day) shows as a missing point.
  const chartData = useMemo(() => {
    const dayMap = new Map<string, number>();
    for (const r of rows) {
      const v = (r.byte_seconds ?? 0) / 86400;
      dayMap.set(r.day, (dayMap.get(r.day) ?? 0) + v);
    }
    return Array.from(dayMap.entries())
      .map(([day, avgBytes]) => ({ day, avgBytes }))
      .sort((a, b) => a.day.localeCompare(b.day));
  }, [rows]);

  // Per-storage-class breakdown — sum byte_seconds and object_count_max
  // across the window. ObjectCountMax is the high-water mark across the
  // window (US-008 v1 sample-once shape) so summing across days gives a
  // representative ordinal, not a true mean.
  const classBreakdown = useMemo(() => {
    const m = new Map<
      string,
      { class: string; byteSeconds: number; objectMax: number; days: number }
    >();
    for (const r of rows) {
      const cur = m.get(r.storage_class) ?? {
        class: r.storage_class,
        byteSeconds: 0,
        objectMax: 0,
        days: 0,
      };
      cur.byteSeconds += r.byte_seconds ?? 0;
      cur.objectMax = Math.max(cur.objectMax, r.object_count_max ?? 0);
      cur.days += 1;
      m.set(r.storage_class, cur);
    }
    return Array.from(m.values()).sort((a, b) => a.class.localeCompare(b.class));
  }, [rows]);

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: queryKeys.buckets.quota(bucket.name) });
    void queryClient.invalidateQueries({ queryKey: ['buckets', 'usage', bucket.name] });
  }

  async function handleDeleteQuota() {
    if (!window.confirm(`Remove all quota caps from ${bucket.name}?`)) return;
    try {
      await deleteBucketQuota(bucket.name);
      void queryClient.invalidateQueries({ queryKey: queryKeys.buckets.quota(bucket.name) });
      showToast({ title: 'Quota removed', description: bucket.name });
    } catch (err) {
      const e = err as Error;
      showToast({
        title: 'Failed to remove quota',
        description: e.message,
        variant: 'destructive',
      });
    }
  }

  return (
    <div className="space-y-4">
      <EditBucketQuotaDialog
        open={editOpen}
        onOpenChange={setEditOpen}
        bucket={bucket.name}
        current={quota}
      />

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div className="space-y-1">
            <CardTitle className="text-base">Current usage</CardTitle>
            <CardDescription>
              Live <code>bucket_stats</code> counters. Quota gates PUT /
              UploadPart / Complete with{' '}
              <code>403 QuotaExceeded</code>.
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={usageQ.isFetching || quotaQ.isFetching}
              aria-label="Refresh usage"
            >
              <RefreshCw
                className={cn(
                  'mr-1.5 h-3.5 w-3.5',
                  (usageQ.isFetching || quotaQ.isFetching) && 'animate-spin',
                )}
                aria-hidden
              />
              Refresh
            </Button>
            {quota && (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="text-destructive hover:text-destructive"
                onClick={handleDeleteQuota}
                aria-label="Remove quota"
              >
                <Trash2 className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                Remove quota
              </Button>
            )}
            <Button type="button" size="sm" onClick={() => setEditOpen(true)}>
              <Pencil className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              Edit quota
            </Button>
          </div>
        </CardHeader>
        <CardContent className="space-y-4">
          {quotaQ.isPending && (
            <Skeleton className="h-3 w-full" />
          )}
          {!quotaQ.isPending && (
            <div className="space-y-2" data-testid="bucket-usage-bar">
              <div className="flex items-center justify-between text-sm">
                <span className="font-medium">Bytes used</span>
                <span className="tabular-nums">
                  {formatBytes(usedBytes)}
                  {maxBytes > 0 && (
                    <>
                      {' '}/ {formatBytes(maxBytes)}{' '}
                      <span className="text-muted-foreground">
                        ({fillPct.toFixed(1)}%)
                      </span>
                    </>
                  )}
                  {!quota && (
                    <span className="ml-2 text-muted-foreground">(no quota set)</span>
                  )}
                </span>
              </div>
              <div className="h-2 w-full overflow-hidden rounded-full bg-muted">
                {quota && maxBytes > 0 ? (
                  <div
                    className={cn(
                      'h-full',
                      fillPct >= 90
                        ? 'bg-destructive'
                        : fillPct >= 75
                          ? 'bg-amber-500'
                          : 'bg-primary',
                    )}
                    style={{ width: `${fillPct}%` }}
                    role="progressbar"
                    aria-valuemin={0}
                    aria-valuemax={100}
                    aria-valuenow={Math.round(fillPct)}
                  />
                ) : (
                  <div
                    className="h-full w-full bg-muted-foreground/20"
                    aria-hidden
                  />
                )}
              </div>
            </div>
          )}
          <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
            <div>
              <div className="text-xs uppercase tracking-wide text-muted-foreground">
                Objects
              </div>
              <div className="mt-1 text-base font-medium tabular-nums">
                {formatCount(usedObjects)}
                {quota && quota.max_objects > 0 && (
                  <span className="ml-1 text-xs text-muted-foreground">
                    / {formatCount(quota.max_objects)}
                  </span>
                )}
              </div>
            </div>
            <div>
              <div className="text-xs uppercase tracking-wide text-muted-foreground">
                Max bytes
              </div>
              <div className="mt-1 text-base font-medium tabular-nums">
                {quota && quota.max_bytes > 0 ? formatBytes(quota.max_bytes) : '—'}
              </div>
            </div>
            <div>
              <div className="text-xs uppercase tracking-wide text-muted-foreground">
                Max objects
              </div>
              <div className="mt-1 text-base font-medium tabular-nums">
                {quota && quota.max_objects > 0
                  ? formatCount(quota.max_objects)
                  : '—'}
              </div>
            </div>
            <div>
              <div className="text-xs uppercase tracking-wide text-muted-foreground">
                Max bytes / object
              </div>
              <div className="mt-1 text-base font-medium tabular-nums">
                {quota && quota.max_bytes_per_object > 0
                  ? formatBytes(quota.max_bytes_per_object)
                  : '—'}
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">30-day usage</CardTitle>
          <CardDescription>
            Average bytes per day across every storage class. Source:
            nightly <code>usage_aggregates</code> rollup
            (<code>byte_seconds / 86400</code>).
          </CardDescription>
        </CardHeader>
        <CardContent>
          {usageQ.isPending && !usageQ.data ? (
            <Skeleton className="h-72 w-full" />
          ) : usageQ.error instanceof Error && !usageQ.data ? (
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">Failed to load usage history</div>
                <div className="text-xs text-destructive/80">
                  {usageQ.error.message}
                </div>
              </div>
            </div>
          ) : chartData.length === 0 ? (
            <div className="py-10 text-center text-sm text-muted-foreground">
              No usage rolled up yet for this window. The nightly worker writes
              one row per (bucket, class, day) at 00:00 UTC.
            </div>
          ) : (
            <div
              className="h-72 w-full"
              data-testid="bucket-usage-chart"
              aria-label="30-day average bytes"
            >
              <ResponsiveContainer width="100%" height="100%">
                <LineChart
                  data={chartData}
                  margin={{ top: 10, right: 16, bottom: 0, left: 0 }}
                >
                  <CartesianGrid
                    strokeDasharray="3 3"
                    stroke="hsl(var(--border))"
                  />
                  <XAxis
                    dataKey="day"
                    fontSize={11}
                    tickLine={false}
                    axisLine={false}
                  />
                  <YAxis
                    fontSize={11}
                    tickLine={false}
                    axisLine={false}
                    tickFormatter={(v: number) => formatBytes(v)}
                    width={70}
                  />
                  <Tooltip
                    formatter={(v: number) => [formatBytes(v), 'avg bytes']}
                    labelFormatter={(l) => l}
                  />
                  <Line
                    type="monotone"
                    dataKey="avgBytes"
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

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Per-storage-class breakdown</CardTitle>
          <CardDescription>
            Aggregated across the same 30-day window.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4 sm:pl-6">Storage class</TableHead>
                  <TableHead className="text-right">Avg bytes / day</TableHead>
                  <TableHead className="text-right">Peak objects</TableHead>
                  <TableHead className="pr-4 text-right sm:pr-6">Days observed</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {usageQ.isPending && !usageQ.data ? (
                  Array.from({ length: 2 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={4} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))
                ) : classBreakdown.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={4} className="py-6 text-center text-muted-foreground">
                      No data yet.
                    </TableCell>
                  </TableRow>
                ) : (
                  classBreakdown.map((c) => (
                    <TableRow key={c.class}>
                      <TableCell className="pl-4 font-medium sm:pl-6">
                        {c.class || 'STANDARD'}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatBytes(c.byteSeconds / 86400 / Math.max(1, c.days))}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatCount(c.objectMax)}
                      </TableCell>
                      <TableCell className="pr-4 text-right tabular-nums sm:pr-6">
                        {c.days}
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
