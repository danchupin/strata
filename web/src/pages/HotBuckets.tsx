import { useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useNavigate } from 'react-router-dom';
import { AlertTriangle, ExternalLink, RefreshCw } from 'lucide-react';

import {
  fetchHotBuckets,
  type AdminApiError,
  type HotBucketSeries,
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
import { Heatmap, type HeatmapClick, type HeatmapRow } from '@/components/Heatmap';

const POLL_INTERVAL_MS = 30_000;

type RangeKey = '15m' | '1h' | '6h' | '24h';
interface RangeOption {
  key: RangeKey;
  label: string;
  range: string;
  step: string;
}

// Step is auto-derived from the range so the matrix stays at ~24-360 cells
// across the time axis (heatmap reads cleanly at that density).
const RANGE_OPTIONS: RangeOption[] = [
  { key: '15m', label: '15m', range: '15m', step: '1m' },
  { key: '1h', label: '1h', range: '1h', step: '5m' },
  { key: '6h', label: '6h', range: '6h', step: '30m' },
  { key: '24h', label: '24h', range: '24h', step: '1h' },
];

function isMetricsUnavailable(err: unknown): boolean {
  return (
    err != null &&
    typeof err === 'object' &&
    (err as AdminApiError).code === 'MetricsUnavailable'
  );
}

function toHeatmapRows(matrix: HotBucketSeries[]): HeatmapRow[] {
  return matrix.map((s) => ({
    label: s.bucket,
    values: s.values.map((p) => ({ ts: p.ts, value: p.value })),
  }));
}

export function HotBucketsPage() {
  const [rangeKey, setRangeKey] = useState<RangeKey>('1h');
  const navigate = useNavigate();
  const opt = useMemo(
    () => RANGE_OPTIONS.find((r) => r.key === rangeKey) ?? RANGE_OPTIONS[1],
    [rangeKey],
  );

  const q = useQuery({
    queryKey: queryKeys.diagnostics.hotBuckets(opt.range, opt.step),
    queryFn: () => fetchHotBuckets({ range: opt.range, step: opt.step }),
    refetchInterval: POLL_INTERVAL_MS,
    placeholderData: keepPreviousData,
    retry: (failureCount, err) => {
      // Don't retry the metrics-unavailable case — it's a config gap, not flaky.
      if (isMetricsUnavailable(err)) return false;
      return failureCount < 1;
    },
    meta: { label: 'hot buckets', silent: true },
  });

  const matrix = q.data?.matrix ?? [];
  const rows = useMemo(() => toHeatmapRows(matrix), [matrix]);
  const promUnavailable = isMetricsUnavailable(q.error);
  const showSkeleton = q.isPending && !q.data && !promUnavailable;
  const otherError =
    !q.data && !promUnavailable && q.error instanceof Error ? q.error.message : null;

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: ['diagnostics', 'hot-buckets'] });
  }

  function handleCellClick(c: HeatmapClick) {
    const since = new Date(c.cellStartTs).toISOString();
    const until = new Date(c.cellEndTs).toISOString();
    const usp = new URLSearchParams({ since, until });
    navigate(
      `/buckets/${encodeURIComponent(c.label)}?${usp.toString()}`,
    );
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Hot buckets</h1>
        <p className="text-sm text-muted-foreground">
          Per-bucket request rate over the selected window. Top 50 buckets,
          1-minute PromQL rate, log-scale color. Click a cell to drill into
          the bucket detail filtered by the cell timespan.
        </p>
      </div>

      {promUnavailable && (
        <Card className="border-amber-500/40 bg-amber-500/5">
          <CardContent className="flex items-start gap-3 py-4 text-sm text-amber-900 dark:text-amber-200">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div className="space-y-1">
              <div className="font-medium">Metrics unavailable</div>
              <p>
                Hot buckets reads from Prometheus. Set{' '}
                <code className="font-mono">STRATA_PROMETHEUS_URL</code> on the
                gateway and restart to enable this view.
              </p>
              <a
                href="/docs/ui.md#prometheus-setup"
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-amber-900 underline-offset-2 hover:underline dark:text-amber-100"
              >
                docs/ui.md#prometheus-setup
                <ExternalLink className="h-3 w-3" aria-hidden />
              </a>
            </div>
          </CardContent>
        </Card>
      )}

      {otherError && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="py-4 text-sm text-destructive">
            <div className="font-medium">Failed to load hot buckets</div>
            <div className="text-xs text-destructive/80">{otherError}</div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
          <div className="space-y-1">
            <CardTitle className="text-base">Request rate per bucket</CardTitle>
            <CardDescription>
              Range {opt.label} · step {opt.step} · auto-refresh 30 s
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <div
              role="group"
              aria-label="Range"
              className="inline-flex rounded-md border border-input bg-background p-0.5 text-sm"
            >
              {RANGE_OPTIONS.map((o) => (
                <button
                  key={o.key}
                  type="button"
                  aria-pressed={rangeKey === o.key}
                  onClick={() => setRangeKey(o.key)}
                  className={cn(
                    'rounded-sm px-2.5 py-1 font-medium transition-colors',
                    rangeKey === o.key
                      ? 'bg-primary text-primary-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  {o.label}
                </button>
              ))}
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching || promUnavailable}
              aria-label="Refresh hot buckets"
            >
              <RefreshCw
                className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
                aria-hidden
              />
              Refresh
            </Button>
          </div>
        </CardHeader>
        <CardContent>
          {promUnavailable ? (
            <div className="flex h-56 items-center justify-center rounded-md border border-dashed border-border/60 bg-muted/30 text-sm text-muted-foreground">
              Heatmap disabled until Prometheus is configured.
            </div>
          ) : showSkeleton ? (
            <Skeleton className="h-72 w-full" />
          ) : (
            <Heatmap rows={rows} onCellClick={handleCellClick} />
          )}
        </CardContent>
      </Card>
    </div>
  );
}
