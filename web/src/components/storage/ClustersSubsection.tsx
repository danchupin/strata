import { useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, AlertTriangle, Loader2 } from 'lucide-react';

import {
  fetchClusterDrainProgress,
  fetchClusters,
  isDrainingState,
  undrainCluster,
  type BucketImpactEntry,
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

import { ActivateClusterModal } from './ActivateClusterModal';
import { BucketReferencesDrawer } from './BucketReferencesDrawer';
import { BulkPlacementFixDialog } from './BulkPlacementFixDialog';
import { CancelDeregisterPrepModal } from './CancelDeregisterPrepModal';
import { ConfirmDrainModal } from './ConfirmDrainModal';
import { ConfirmUndrainEvacuationModal } from './ConfirmUndrainEvacuationModal';
import { DrainProgressBar } from './DrainProgressBar';
import { LiveClusterWeightSlider } from './LiveClusterWeightSlider';
import { RebalanceProgressChip } from './RebalanceProgressChip';
import {
  clusterCardAction,
  undrainDisabledTooltip,
} from './clusterCardAction';

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

// clusterStateClass + clusterStateLabel mirror the 5-state machine
// (drain-transparency 4-state + cluster-weights US-001 `pending`). The
// card's state badge colors track mode: readonly → orange (reversible
// stop-write), evacuating → red (decommission in progress), live →
// emerald, pending → muted gray (not receiving writes yet), removed →
// muted. Legacy `draining` rows fall back to amber until the meta layer
// normalises.
function clusterStateClass(state: string): string {
  switch (state.toLowerCase()) {
    case 'live':
      return 'bg-emerald-500/15 text-emerald-700 border-emerald-500/30 dark:text-emerald-300';
    case 'pending':
      return 'bg-muted text-muted-foreground border-border';
    case 'draining_readonly':
      return 'bg-orange-500/15 text-orange-800 border-orange-500/30 dark:text-orange-300';
    case 'evacuating':
      return 'bg-red-500/15 text-red-800 border-red-500/30 dark:text-red-300';
    case 'draining':
      return 'bg-amber-500/15 text-amber-800 border-amber-500/30 dark:text-amber-300';
    case 'removed':
      return 'bg-muted text-muted-foreground border-border';
    default:
      return 'bg-muted text-muted-foreground border-border';
  }
}

function clusterStateLabel(state: string): string {
  switch (state.toLowerCase()) {
    case 'pending':
      return 'Pending — not receiving writes';
    case 'draining_readonly':
      return 'stop-writes';
    case 'evacuating':
      return 'evacuating';
    case '':
      return '—';
    default:
      return state.toLowerCase();
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
}

function ClusterCard({ cluster, usage }: ClusterCardProps) {
  const [drainOpen, setDrainOpen] = useState(false);
  const [activateOpen, setActivateOpen] = useState(false);
  const [refsOpen, setRefsOpen] = useState(false);
  const [undraining, setUndraining] = useState(false);
  const [bulkFixOpen, setBulkFixOpen] = useState(false);
  const [bulkFixStuck, setBulkFixStuck] = useState<BucketImpactEntry[]>([]);
  const [cancelPrepOpen, setCancelPrepOpen] = useState(false);
  const [undrainEvacOpen, setUndrainEvacOpen] = useState(false);

  const stateLower = cluster.state.toLowerCase();
  const isLive = stateLower === 'live';
  const isDraining = isDrainingState(cluster.state);
  const isReadonlyDrain = stateLower === 'draining_readonly';
  const supportsFill = cluster.backend.toLowerCase() === 'rados';
  const hasFill = supportsFill && usage?.hasUsage;

  // Share the drain-progress query with DrainProgressBar (same key) so
  // the migrating-banner can drop when chunks_on_cluster=0 without an
  // extra round-trip (US-006 drain-cleanup). The same query feeds the
  // state-aware action button selector (US-007 drain-cleanup).
  const drainProgressQ = useQuery({
    queryKey: queryKeys.clusterDrainProgress(cluster.id),
    queryFn: () => fetchClusterDrainProgress(cluster.id),
    enabled: isDraining && !isReadonlyDrain,
    refetchInterval: CLUSTERS_POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: `drain progress ${cluster.id}`, silent: true },
  });
  const drainChunks = drainProgressQ.data?.chunks_on_cluster ?? null;
  const hasChunksToMigrate = drainChunks != null && drainChunks > 0;
  const deregisterReady = drainProgressQ.data?.deregister_ready === true;
  const notReadyReasons = drainProgressQ.data?.not_ready_reasons ?? [];
  const action = clusterCardAction({
    state: cluster.state,
    chunks: drainChunks,
    deregisterReady,
    notReadyReasons,
  });

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
              {clusterStateLabel(cluster.state)}
            </Badge>
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
        {cluster.backend.toLowerCase() !== 'memory' && stateLower !== 'pending' && (
          <RebalanceProgressChip clusterID={cluster.id} />
        )}
        {isLive && (
          <LiveClusterWeightSlider clusterID={cluster.id} weight={cluster.weight} />
        )}
        {isDraining && (
          <>
            <DrainProgressBar
              clusterID={cluster.id}
              onUpgradeToEvacuate={
                isReadonlyDrain ? () => setDrainOpen(true) : undefined
              }
              onUndrain={isReadonlyDrain ? handleUndrain : undefined}
              undraining={undraining}
            />
            {!isReadonlyDrain && hasChunksToMigrate && (
              <div
                className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/5 p-2 text-xs text-amber-800 dark:text-amber-300"
                data-testid="cluster-card-migrating-banner"
              >
                <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
                <span>Rebalance worker is migrating chunks off this cluster.</span>
              </div>
            )}
          </>
        )}
        <div className="flex items-center justify-between gap-2">
          <button
            type="button"
            onClick={() => setRefsOpen(true)}
            className="text-xs text-primary hover:underline"
          >
            Show affected buckets
          </button>
          <ActionButton
            action={action}
            undraining={undraining}
            notReadyReasons={notReadyReasons}
            onActivate={() => setActivateOpen(true)}
            onDrain={() => setDrainOpen(true)}
            onUndrainEvacuation={() => setUndrainEvacOpen(true)}
            onCancelDeregisterPrep={() => setCancelPrepOpen(true)}
          />
        </div>
      </CardContent>
      <ActivateClusterModal
        open={activateOpen}
        onOpenChange={setActivateOpen}
        clusterID={cluster.id}
      />
      <ConfirmDrainModal
        open={drainOpen}
        onOpenChange={setDrainOpen}
        clusterID={cluster.id}
        currentState={cluster.state}
        onOpenBulkFix={(stuck) => {
          setBulkFixStuck(stuck);
          setBulkFixOpen(true);
        }}
      />
      <BulkPlacementFixDialog
        open={bulkFixOpen}
        onOpenChange={setBulkFixOpen}
        clusterID={cluster.id}
        stuck={bulkFixStuck}
      />
      <BucketReferencesDrawer
        open={refsOpen}
        onOpenChange={setRefsOpen}
        clusterID={cluster.id}
        onOpenBulkFix={(stuck) => {
          setBulkFixStuck(stuck);
          setBulkFixOpen(true);
        }}
      />
      <CancelDeregisterPrepModal
        open={cancelPrepOpen}
        onOpenChange={setCancelPrepOpen}
        clusterID={cluster.id}
      />
      <ConfirmUndrainEvacuationModal
        open={undrainEvacOpen}
        onOpenChange={setUndrainEvacOpen}
        clusterID={cluster.id}
      />
    </Card>
  );
}

interface ActionButtonProps {
  action: ReturnType<typeof clusterCardAction>;
  undraining: boolean;
  notReadyReasons: string[];
  onActivate: () => void;
  onDrain: () => void;
  onUndrainEvacuation: () => void;
  onCancelDeregisterPrep: () => void;
}

// ActionButton renders the bottom-right action button on the cluster
// card per the State Truth Table (US-007 drain-cleanup). Returns null
// for the `none` cell — the draining_readonly path delegates Upgrade +
// Undrain to DrainProgressBar so we don't double-render the controls.
function ActionButton({
  action,
  undraining,
  notReadyReasons,
  onActivate,
  onDrain,
  onUndrainEvacuation,
  onCancelDeregisterPrep,
}: ActionButtonProps) {
  switch (action) {
    case 'activate':
      return (
        <Button
          type="button"
          variant="default"
          size="sm"
          onClick={onActivate}
          data-testid="cluster-card-activate"
        >
          Activate
        </Button>
      );
    case 'drain':
      return (
        <Button
          type="button"
          variant="destructive"
          size="sm"
          onClick={onDrain}
          data-testid="cluster-card-drain"
        >
          Drain
        </Button>
      );
    case 'drain-disabled':
      return (
        <Button
          type="button"
          variant="destructive"
          size="sm"
          disabled
          data-testid="cluster-card-drain-disabled"
        >
          Drain
        </Button>
      );
    case 'undrain-confirm-evacuation':
      return (
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={onUndrainEvacuation}
          disabled={undraining}
          data-testid="cluster-card-undrain-evacuation"
        >
          {undraining && (
            <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
          )}
          Undrain (cancel evacuation)
        </Button>
      );
    case 'undrain-disabled-gc':
      return (
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled
          title={undrainDisabledTooltip(notReadyReasons)}
          data-testid="cluster-card-undrain-disabled"
        >
          Undrain
        </Button>
      );
    case 'cancel-deregister-prep':
      return (
        <Button
          type="button"
          variant="destructive"
          size="sm"
          onClick={onCancelDeregisterPrep}
          data-testid="cluster-card-cancel-deregister-prep"
        >
          Cancel deregister prep
        </Button>
      );
    case 'none':
      return null;
    default:
      return null;
  }
}
