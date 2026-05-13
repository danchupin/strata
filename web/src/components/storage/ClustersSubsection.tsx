import { useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, AlertTriangle, Loader2 } from 'lucide-react';

import {
  fetchClusters,
  undrainCluster,
  type ClusterStateEntry,
  type PoolStatus,
} from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

import { ConfirmDrainModal } from './ConfirmDrainModal';
import { DrainProgressBar } from './DrainProgressBar';
import { RebalanceProgressChip } from './RebalanceProgressChip';

const CLUSTERS_POLL_MS = 10_000;

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  let v = bytes;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function clusterStateClass(state: string): string {
  switch (state.toLowerCase()) {
    case 'live':
      return 'bg-emerald-500/15 text-emerald-700 border-emerald-500/30 dark:text-emerald-300';
    case 'draining':
      return 'bg-amber-500/15 text-amber-800 border-amber-500/30 dark:text-amber-300';
    case 'removed':
      return 'bg-muted text-muted-foreground border-border';
    default:
      return 'bg-muted text-muted-foreground border-border';
  }
}

interface Props {
  // pools is the per-pool slice from /admin/v1/storage/data. The
  // subsection aggregates BytesUsed by PoolStatus.Cluster to render
  // each cluster card's "used bytes" — backends that don't expose
  // pool-level fill (s3) hand back rows with bytes_used=0 and the
  // card surfaces "n/a" with a tooltip.
  pools: PoolStatus[];
}

interface AggregatedUsage {
  hasUsage: boolean;
  bytesUsed: number;
}

export function ClustersSubsection({ pools }: Props) {
  const q = useQuery({
    queryKey: queryKeys.clusters,
    queryFn: fetchClusters,
    refetchInterval: CLUSTERS_POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'clusters' },
  });

  const clusters = q.data?.clusters ?? [];
  const drainStrict = Boolean(q.data?.drainStrict);
  const showSkeleton = q.isPending && !q.data;
  const errMsg = !q.data && q.error instanceof Error ? q.error.message : null;

  // Aggregate used bytes per (cluster, backend) from PoolStatus. RADOS
  // pool rows carry both class+cluster+bytes_used so the cluster card
  // surfaces sum(bytes_used) per cluster id. S3 rows leave bytes_used
  // at 0 (no per-cluster fill telemetry); the card shows "n/a".
  const usageByCluster = useMemo<Map<string, AggregatedUsage>>(() => {
    const m = new Map<string, AggregatedUsage>();
    for (const p of pools) {
      const id = p.cluster ?? '';
      if (!id) continue;
      const prev = m.get(id) ?? { hasUsage: false, bytesUsed: 0 };
      const next = {
        hasUsage: prev.hasUsage || p.bytes_used > 0,
        bytesUsed: prev.bytesUsed + (Number.isFinite(p.bytes_used) ? p.bytes_used : 0),
      };
      m.set(id, next);
    }
    return m;
  }, [pools]);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Clusters</CardTitle>
        <CardDescription>
          Registered backend clusters and their drain state. Drain stops new
          PUTs and migrates existing chunks off the cluster via the rebalance
          worker.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {errMsg && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load clusters</div>
              <div className="text-xs text-destructive/80">{errMsg}</div>
            </div>
          </div>
        )}
        {showSkeleton ? (
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {Array.from({ length: 2 }).map((_, i) => (
              <Skeleton key={i} className="h-32 w-full" />
            ))}
          </div>
        ) : clusters.length === 0 ? (
          <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
            No clusters registered. Configure{' '}
            <code className="rounded bg-muted px-1 text-xs">
              STRATA_RADOS_CLUSTERS
            </code>{' '}
            or{' '}
            <code className="rounded bg-muted px-1 text-xs">
              STRATA_S3_CLUSTERS
            </code>{' '}
            to surface entries.
          </div>
        ) : (
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {clusters.map((c) => (
              <ClusterCard
                key={c.id}
                cluster={c}
                usage={usageByCluster.get(c.id)}
                drainStrict={drainStrict}
              />
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

interface ClusterCardProps {
  cluster: ClusterStateEntry;
  usage: AggregatedUsage | undefined;
  drainStrict: boolean;
}

function ClusterCard({ cluster, usage, drainStrict }: ClusterCardProps) {
  const [drainOpen, setDrainOpen] = useState(false);
  const [undraining, setUndraining] = useState(false);

  const isDraining = cluster.state.toLowerCase() === 'draining';
  const supportsFill = cluster.backend.toLowerCase() === 'rados';
  const hasFill = supportsFill && usage?.hasUsage;

  async function handleUndrain() {
    setUndraining(true);
    try {
      await undrainCluster(cluster.id);
      showToast({
        title: `Cluster ${cluster.id} undrained`,
        description: 'New PUTs may target it again once the cache TTL elapses.',
      });
      void queryClient.invalidateQueries({ queryKey: queryKeys.clusters });
    } catch (err) {
      showToast({
        title: 'Undrain failed',
        description: err instanceof Error ? err.message : String(err),
        variant: 'destructive',
      });
    } finally {
      setUndraining(false);
    }
  }

  return (
    <Card className="relative">
      <CardHeader className="space-y-1 pb-3">
        <CardTitle className="flex items-center justify-between gap-2 font-mono text-sm">
          <span className="truncate" title={cluster.id}>
            {cluster.id}
          </span>
          <span className="flex shrink-0 items-center gap-1">
            <Badge
              variant="outline"
              className={cn('font-medium', clusterStateClass(cluster.state))}
            >
              {cluster.state || '—'}
            </Badge>
            {drainStrict && (
              <Badge
                variant="outline"
                className="border-amber-500/40 bg-amber-500/10 text-[10px] font-medium uppercase text-amber-800 dark:text-amber-300"
                title="STRATA_DRAIN_STRICT=on: PUTs that fall back to a draining cluster are refused with 503 DrainRefused"
              >
                strict
              </Badge>
            )}
          </span>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant="outline" className="font-mono text-[10px] uppercase">
            {cluster.backend || 'unknown'}
          </Badge>
          {hasFill ? (
            <span className="tabular-nums text-muted-foreground">
              used {formatBytes(usage?.bytesUsed ?? 0)}
            </span>
          ) : (
            <span
              className="text-muted-foreground"
              title={
                supportsFill
                  ? 'No per-pool bytes_used reported yet — bucketstats worker seeds this on the next pass.'
                  : 'S3 backend has no cluster fill telemetry'
              }
            >
              n/a
            </span>
          )}
        </div>
        {cluster.backend.toLowerCase() !== 'memory' && (
          <RebalanceProgressChip clusterID={cluster.id} />
        )}
        {isDraining && (
          <>
            <DrainProgressBar clusterID={cluster.id} />
            <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/5 p-2 text-xs text-amber-800 dark:text-amber-300">
              <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
              <span>Rebalance worker is migrating chunks off this cluster.</span>
            </div>
          </>
        )}
        <div className="flex justify-end">
          {isDraining ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleUndrain}
              disabled={undraining}
            >
              {undraining && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Undrain
            </Button>
          ) : (
            <Button
              type="button"
              variant="destructive"
              size="sm"
              onClick={() => setDrainOpen(true)}
              disabled={cluster.state.toLowerCase() === 'removed'}
            >
              Drain
            </Button>
          )}
        </div>
      </CardContent>
      <ConfirmDrainModal
        open={drainOpen}
        onOpenChange={setDrainOpen}
        clusterID={cluster.id}
      />
    </Card>
  );
}
