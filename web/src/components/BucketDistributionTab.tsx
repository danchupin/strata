import { useMemo } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertTriangle, ExternalLink, RefreshCw } from 'lucide-react';
import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';

import {
  fetchBucketDistribution,
  type BucketDetail,
  type BucketShardStat,
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
// Skew banner threshold: max(shard) / median(shard) > 5 means one shard is at
// least 5x the typical shard load — operator-actionable signal that the bucket
// should be resharded. Mirrors the AC literally; tweak only with PRD-side
// guidance.
const SKEW_RATIO_THRESHOLD = 5;

interface Props {
  bucket: BucketDetail;
}

interface ChartRow {
  shard: number;
  bytes: number;
  objects: number;
}

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

// computeSkewRatio returns max(values) / median(non-zero values). A bucket
// with no live objects (every shard zero) yields 0 — the banner stays hidden.
// median uses the half of shards that actually carry data so a sparsely
// populated bucket whose 60 of 64 shards are zero doesn't produce an absurd
// "infinite skew" signal.
function computeSkewRatio(values: number[]): number {
  if (values.length === 0) return 0;
  const max = Math.max(...values);
  if (max <= 0) return 0;
  const nonZero = values.filter((v) => v > 0).sort((a, b) => a - b);
  if (nonZero.length === 0) return 0;
  const mid = Math.floor(nonZero.length / 2);
  const median =
    nonZero.length % 2 === 1
      ? nonZero[mid]
      : (nonZero[mid - 1] + nonZero[mid]) / 2;
  if (median <= 0) return 0;
  return max / median;
}

export function BucketDistributionTab({ bucket }: Props) {
  const q = useQuery({
    queryKey: queryKeys.buckets.distribution(bucket.name),
    queryFn: () => fetchBucketDistribution(bucket.name),
    refetchInterval: POLL_INTERVAL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'bucket distribution' },
  });

  const shards: BucketShardStat[] = q.data?.shards ?? [];
  const rows = useMemo<ChartRow[]>(
    () =>
      shards.map((s) => ({
        shard: s.shard,
        bytes: s.bytes,
        objects: s.objects,
      })),
    [shards],
  );

  const byteSkew = useMemo(
    () => computeSkewRatio(rows.map((r) => r.bytes)),
    [rows],
  );
  const objectSkew = useMemo(
    () => computeSkewRatio(rows.map((r) => r.objects)),
    [rows],
  );
  const showSkew =
    byteSkew > SKEW_RATIO_THRESHOLD || objectSkew > SKEW_RATIO_THRESHOLD;
  const showSkeleton = q.isPending && !q.data;
  const errorMessage =
    !q.data && q.error instanceof Error ? q.error.message : null;

  function handleRefresh() {
    void queryClient.invalidateQueries({
      queryKey: queryKeys.buckets.distribution(bucket.name),
    });
  }

  return (
    <div className="space-y-4">
      {showSkew && (
        <Card className="border-amber-500/40 bg-amber-500/5">
          <CardContent className="flex items-start gap-3 py-4 text-sm text-amber-900 dark:text-amber-200">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div className="space-y-1">
              <div className="font-medium">
                Shard skew detected — consider resharding
              </div>
              <p>
                Top shard is{' '}
                <span className="font-mono">
                  {Math.max(byteSkew, objectSkew).toFixed(1)}×
                </span>{' '}
                the median (threshold {SKEW_RATIO_THRESHOLD}×). Hot shards
                concentrate write load on a single Cassandra/TiKV partition.
              </p>
              <a
                href="/docs/ui.md#bucket-resharding"
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-amber-900 underline-offset-2 hover:underline dark:text-amber-100"
              >
                docs/ui.md#bucket-resharding
                <ExternalLink className="h-3 w-3" aria-hidden />
              </a>
            </div>
          </CardContent>
        </Card>
      )}

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="py-4 text-sm text-destructive">
            <div className="font-medium">Failed to load distribution</div>
            <div className="text-xs text-destructive/80">{errorMessage}</div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
          <div className="space-y-1">
            <CardTitle className="text-base">Shard distribution</CardTitle>
            <CardDescription>
              {bucket.shard_count} shards · live (latest non-delete-marker)
              version totals · auto-refresh 30 s.
            </CardDescription>
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={handleRefresh}
            disabled={q.isFetching}
            aria-label="Refresh distribution"
          >
            <RefreshCw
              className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
              aria-hidden
            />
            Refresh
          </Button>
        </CardHeader>
        <CardContent>
          {showSkeleton ? (
            <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
              <Skeleton className="h-72 w-full" />
              <Skeleton className="h-72 w-full" />
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
              <DistributionChart
                title="Bytes per shard"
                ariaLabel="Bytes per shard"
                rows={rows}
                dataKey="bytes"
                color="hsl(220 90% 56%)"
                tooltipFormatter={(v) => [formatBytes(v), 'bytes']}
                tickFormatter={formatBytes}
              />
              <DistributionChart
                title="Objects per shard"
                ariaLabel="Objects per shard"
                rows={rows}
                dataKey="objects"
                color="hsl(160 70% 40%)"
                tooltipFormatter={(v) => [v.toLocaleString(), 'objects']}
                tickFormatter={(v) =>
                  Number.isFinite(v) ? Math.round(v).toLocaleString() : ''
                }
              />
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

interface ChartProps {
  title: string;
  ariaLabel: string;
  rows: ChartRow[];
  dataKey: 'bytes' | 'objects';
  color: string;
  tooltipFormatter: (v: number) => [string, string];
  tickFormatter: (v: number) => string;
}

function DistributionChart({
  title,
  ariaLabel,
  rows,
  dataKey,
  color,
  tooltipFormatter,
  tickFormatter,
}: ChartProps) {
  const hasData = rows.some((r) => r[dataKey] > 0);
  return (
    <div className="space-y-2">
      <div className="text-xs uppercase tracking-wide text-muted-foreground">
        {title}
      </div>
      {!hasData ? (
        <div className="flex h-72 items-center justify-center rounded-md border border-dashed border-border/60 bg-muted/30 text-sm text-muted-foreground">
          No live objects in this bucket.
        </div>
      ) : (
        <div className="h-72 w-full" aria-label={ariaLabel}>
          <ResponsiveContainer width="100%" height="100%">
            <BarChart
              data={rows}
              margin={{ top: 8, right: 16, left: 0, bottom: 0 }}
            >
              <CartesianGrid strokeDasharray="3 3" className="stroke-border/40" />
              <XAxis
                dataKey="shard"
                stroke="currentColor"
                className="text-xs text-muted-foreground"
                interval="preserveStartEnd"
              />
              <YAxis
                scale="log"
                domain={[0.5, 'auto']}
                allowDataOverflow
                tickFormatter={tickFormatter}
                stroke="currentColor"
                className="text-xs text-muted-foreground"
                width={64}
              />
              <Tooltip
                formatter={(v: number) => tooltipFormatter(v)}
                labelFormatter={(label) => `shard ${label}`}
              />
              <Bar dataKey={dataKey} fill={color} isAnimationActive={false} />
            </BarChart>
          </ResponsiveContainer>
        </div>
      )}
    </div>
  );
}

export default BucketDistributionTab;
