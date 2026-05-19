import { useQuery } from '@tanstack/react-query';
import { ExternalLink, Gauge } from 'lucide-react';

import {
  fetchRebalanceBandwidth,
  fetchRebalanceConfig,
  type RebalanceBandwidth,
  type RebalanceConfig,
} from '@/api/client';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { queryKeys } from '@/lib/query';

const BANDWIDTH_POLL_MS = 30_000;
const TUNE_DOCS_URL =
  '/docs/best-practices/placement-rebalance/#bandwidth-tuning';
const TOKEN_MATH_TOOLTIP =
  'Each chunk move spends chunkSize × 2 tokens (read from source + write to target). Aggregate = per-replica rate × replicas. Effective forward = aggregate ÷ 2 (net new bytes landing on the target).';

function formatMBs(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '0';
  if (n >= 100) return n.toFixed(0);
  if (n >= 10) return n.toFixed(1);
  return n.toFixed(2);
}

function formatRate(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '0';
  if (n >= 10) return n.toFixed(1);
  return n.toFixed(2);
}

// RebalanceConfigCard surfaces the rebalance worker tunables + the
// cluster-wide live bandwidth roll-up on the Cluster Overview page
// (US-003 drain-rebalance-transparency). Reads `rebalance-config`
// (TanStack key shared with <DrainProgressBar>) + `rebalance-bandwidth`
// (this card owns the key). Hidden entirely on rebalance-config 404 so
// a legacy gateway doesn't render an empty card.
export function RebalanceConfigCard() {
  const cfgQuery = useQuery({
    queryKey: queryKeys.rebalanceConfig,
    queryFn: fetchRebalanceConfig,
    staleTime: Infinity,
    refetchInterval: false,
    refetchOnWindowFocus: false,
    retry: false,
    meta: { label: 'rebalance-config', silent: true },
  });
  const bwQuery = useQuery({
    queryKey: queryKeys.rebalanceBandwidth,
    queryFn: fetchRebalanceBandwidth,
    refetchInterval: BANDWIDTH_POLL_MS,
    retry: false,
    meta: { label: 'rebalance-bandwidth', silent: true },
  });

  // Hide the entire card when the rebalance-config endpoint is unavailable
  // (404 on a legacy gateway, 5xx). The AC explicitly rules out an error
  // toast — the operator sees no card rather than a broken one.
  if (cfgQuery.isError) return null;

  if (cfgQuery.isPending && !cfgQuery.data) {
    return (
      <Card>
        <CardHeader>
          <Skeleton className="h-5 w-44" />
          <Skeleton className="mt-2 h-4 w-64" />
        </CardHeader>
        <CardContent>
          <Skeleton className="h-20 w-full" />
        </CardContent>
      </Card>
    );
  }

  const cfg: RebalanceConfig | undefined = cfgQuery.data;
  if (!cfg) return null;

  const aggregate = cfg.rate_mb_s * cfg.replicas_count;
  const effective = aggregate / 2;
  const bw: RebalanceBandwidth | undefined = bwQuery.data;
  const showBandwidth = bw != null && bw.metrics_available === true;
  const observedMBs = bw ? bw.bytes_per_sec / (1024 * 1024) : 0;

  return (
    <Card data-testid="rebalance-config-card">
      <CardHeader className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
        <div className="space-y-1">
          <CardTitle className="flex items-center gap-2 text-base">
            <Gauge className="h-4 w-4" aria-hidden />
            Rebalance bandwidth
          </CardTitle>
          <CardDescription>
            Per-replica rate cap, cluster-wide aggregate, and live
            observed bandwidth across all destination clusters.
          </CardDescription>
        </div>
        <a
          href={TUNE_DOCS_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground underline-offset-2 hover:underline"
          data-testid="rebalance-tune-link"
        >
          Tune
          <ExternalLink className="h-3 w-3" aria-hidden />
        </a>
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        <dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 tabular-nums">
          <dt className="text-muted-foreground">Per-replica rate</dt>
          <dd className="font-medium" data-testid="rebalance-per-replica">
            {cfg.rate_mb_s} MB/s
          </dd>
          <dt
            className="text-muted-foreground"
            title={TOKEN_MATH_TOOLTIP}
            data-testid="rebalance-aggregate-label"
          >
            Aggregate (× {cfg.replicas_count}{' '}
            {cfg.replicas_count === 1 ? 'replica' : 'replicas'})
          </dt>
          <dd
            className="font-medium"
            data-testid="rebalance-aggregate"
            title={TOKEN_MATH_TOOLTIP}
          >
            ~{aggregate} MB/s
          </dd>
          <dt
            className="text-muted-foreground"
            title={TOKEN_MATH_TOOLTIP}
            data-testid="rebalance-effective-label"
          >
            Effective forward
          </dt>
          <dd
            className="font-medium"
            data-testid="rebalance-effective"
            title={TOKEN_MATH_TOOLTIP}
          >
            ~{effective} MB/s
          </dd>
          <dt className="text-muted-foreground">Cadence</dt>
          <dd className="font-medium" data-testid="rebalance-cadence">
            every {cfg.interval_seconds}s · Inflight: {cfg.inflight} · Shards:{' '}
            {cfg.shards}
          </dd>
        </dl>
        {showBandwidth && (
          <div
            className="rounded-md border border-border/60 bg-muted/30 px-2 py-1.5 text-xs text-muted-foreground tabular-nums"
            data-testid="rebalance-observed-row"
          >
            <span className="text-foreground">Observed last 1m: </span>
            <span data-testid="rebalance-observed-mbs">
              ~{formatMBs(observedMBs)} MB/s
            </span>{' '}
            ·{' '}
            <span data-testid="rebalance-observed-chunks">
              ~{formatRate(bw.chunks_per_sec)} chunks/sec
            </span>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
