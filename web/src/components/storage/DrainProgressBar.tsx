import { useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, CheckCircle2, Loader2 } from 'lucide-react';

import {
  fetchClusterDrainProgress,
  fetchClusterRebalanceProgress,
  fetchGCConfig,
  fetchRebalanceConfig,
  type BucketDrainProgressEntry,
  type GCConfig,
  type RebalanceConfig,
  DRAIN_NOT_READY_REASON_LABELS,
} from '@/api/client';
import { Button } from '@/components/ui/button';
import { queryKeys } from '@/lib/query';
import { cn } from '@/lib/utils';

import { StuckBucketsDrawer } from './StuckBucketsDrawer';

const DRAIN_PROGRESS_POLL_MS = 30_000;
const REBALANCE_POLL_MS = 30_000;
// RADOS stores data as 4 MiB chunks; the rebalance ETA formula multiplies
// remaining chunk count by this constant to estimate bytes-to-move.
const CHUNK_SIZE_BYTES = 4 * 1024 * 1024;
const ETA_CAP_SECONDS = 24 * 3600;

interface Props {
  clusterID: string;
  // Wired from the parent ClusterCard so readonly-mode buttons can flip
  // the cluster state without DrainProgressBar owning the modal stack.
  onUpgradeToEvacuate?: () => void;
  onUndrain?: () => void;
  undraining?: boolean;
}

function formatCount(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return n.toFixed(0);
}

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

const PHYSICAL_UNAVAILABLE_TOOLTIP =
  'physical count unavailable on this backend';
const AWAITING_GC_TOOLTIP_FALLBACK =
  'Physical delete completes after STRATA_GC_GRACE elapses (~5m default) plus the next gc worker tick.';

function clampPositive(n: number): number {
  return n > 0 ? n : 1;
}

// computeAwaitingGCEtaMinutes mirrors the formula laid out in US-002:
//   eta_min = ceil(grace_seconds / 60)
//             + ceil(gc_queue_pending
//                    / (batch_size × shards × 1)
//                    × (interval_seconds / 60))
// Denominator components are clamped to 1 when zero so an operator with a
// degenerate env doesn't divide-by-zero. Returns Infinity when the inputs
// project past the 24h cap so callers render the `~24h+` label.
function computeAwaitingGCEtaMinutes(
  gcQueuePending: number,
  cfg: GCConfig,
): number {
  const graceMin = Math.ceil(cfg.grace_seconds / 60);
  const batch = clampPositive(cfg.batch_size);
  const shards = clampPositive(cfg.shards);
  const intervalMin = cfg.interval_seconds / 60;
  const queue = Math.max(0, gcQueuePending);
  const ticks = queue / (batch * shards);
  const queueMin = Math.ceil(ticks * intervalMin);
  const total = graceMin + queueMin;
  if (!Number.isFinite(total) || total > ETA_CAP_SECONDS / 60) {
    return Infinity;
  }
  return total;
}

function formatMinutes(min: number): string {
  if (!Number.isFinite(min) || min <= 0) return '~0m';
  if (min < 60) return `~${Math.round(min)}m`;
  const h = Math.floor(min / 60);
  const m = Math.round(min - h * 60);
  return m > 0 ? `~${h}h ${m}m` : `~${h}h`;
}

function formatEtaMinutesCap(min: number): string {
  if (!Number.isFinite(min) || min > ETA_CAP_SECONDS / 60) return '~24h+ ETA';
  return `${formatMinutes(min)} ETA`;
}

function formatMBs(mbs: number): string {
  if (!Number.isFinite(mbs) || mbs < 0) return '0';
  if (mbs >= 100) return mbs.toFixed(0);
  if (mbs >= 10) return mbs.toFixed(1);
  return mbs.toFixed(2);
}

// computeMigratingEtaSeconds:
//   Z = (remaining × chunk_size) / observed_rate     if observed > 0
//   else (remaining × chunk_size) / (rate_mb_s × replicas_count / 2)
// rate_mb_s × replicas_count / 2 is the aggregate effective forward
// estimate from the runbook (chunk move costs read+write tokens).
// Returns Infinity when the fallback denominator is 0 so the chip caps
// at `~24h+`.
function computeMigratingEtaSeconds(
  remainingChunks: number,
  observedBytesPerSec: number,
  cfg: RebalanceConfig,
): number {
  const remaining = Math.max(0, remainingChunks);
  if (remaining <= 0) return 0;
  const bytes = remaining * CHUNK_SIZE_BYTES;
  if (observedBytesPerSec > 0) {
    return bytes / observedBytesPerSec;
  }
  const fallbackBytesPerSec =
    (cfg.rate_mb_s * cfg.replicas_count * 1024 * 1024) / 2;
  if (fallbackBytesPerSec <= 0) return Infinity;
  return bytes / fallbackBytesPerSec;
}

function formatEta(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds) || seconds <= 0) {
    return '?';
  }
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
  if (seconds < 86_400) {
    const h = seconds / 3600;
    return `${h >= 10 ? h.toFixed(0) : h.toFixed(1)}h`;
  }
  const d = seconds / 86_400;
  return `${d >= 10 ? d.toFixed(0) : d.toFixed(1)}d`;
}

// DrainProgressBar renders the per-cluster drain status block surfaced on
// each draining cluster card. Reads from GET /admin/v1/clusters/{id}
// /drain-progress every 30s via the shared query key. Render shapes:
//   - mode=readonly → stop-writes chip + Upgrade + Undrain buttons
//   - mode=evacuate + scan pending → muted "Scan pending" line
//   - mode=evacuate + chunks > 0 → mode label + three categorized
//     counters (Migrating / Stuck single-policy / Stuck no-policy);
//     stuck counters open <StuckBucketsDrawer>. Progress bar + ETA cell
//     when base_chunks_at_start known and migratable > 0.
//   - mode=evacuate + chunks == 0 + deregister_ready → green
//     "Ready to deregister" chip.
//
// The "(stale)" suffix + muted text fire when the response includes the
// "progress data stale" warning (cache older than 2×STRATA_REBALANCE_INTERVAL).
export function DrainProgressBar({
  clusterID,
  onUpgradeToEvacuate,
  onUndrain,
  undraining = false,
}: Props) {
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [drawerCategory, setDrawerCategory] = useState<
    'stuck_single_policy' | 'stuck_no_policy' | null
  >(null);

  const q = useQuery({
    queryKey: queryKeys.clusterDrainProgress(clusterID),
    queryFn: () => fetchClusterDrainProgress(clusterID),
    refetchInterval: DRAIN_PROGRESS_POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: `drain progress ${clusterID}`, silent: true },
  });
  // gc-config + rebalance-config are env-static at boot (US-001) — fetch once
  // per app session, never refetch. Silent meta so 404 on a legacy gateway
  // doesn't surface a toast; the chip falls back to the pre-cycle copy.
  const gcCfgQuery = useQuery({
    queryKey: queryKeys.gcConfig,
    queryFn: fetchGCConfig,
    staleTime: Infinity,
    refetchInterval: false,
    refetchOnWindowFocus: false,
    retry: false,
    meta: { label: 'gc-config', silent: true },
  });
  const rebCfgQuery = useQuery({
    queryKey: queryKeys.rebalanceConfig,
    queryFn: fetchRebalanceConfig,
    staleTime: Infinity,
    refetchInterval: false,
    refetchOnWindowFocus: false,
    retry: false,
    meta: { label: 'rebalance-config', silent: true },
  });
  // rebalance-progress feeds the Migrating chip's observed-bandwidth row
  // (the existing RebalanceProgressChip shares this key so a card with
  // both mounted issues one fetch).
  const rebProgressQuery = useQuery({
    queryKey: queryKeys.clusterRebalance(clusterID),
    queryFn: () => fetchClusterRebalanceProgress(clusterID),
    refetchInterval: REBALANCE_POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: `rebalance progress ${clusterID}`, silent: true },
  });

  if (q.isPending && !q.data) {
    return (
      <div className="text-xs text-muted-foreground">Loading drain progress…</div>
    );
  }
  const data = q.data;
  if (!data) {
    return null;
  }

  const mode = (data.mode ?? '').toLowerCase();
  const isReadonly = mode === 'readonly';
  const stale = (data.warnings ?? []).includes('progress data stale');
  const pending = (data.warnings ?? []).some((w) =>
    w.startsWith('progress scan pending'),
  );
  const chunks = data.chunks_on_cluster ?? null;
  const migratable = data.migratable_chunks ?? 0;
  const stuckSingle = data.stuck_single_policy_chunks ?? 0;
  const stuckNo = data.stuck_no_policy_chunks ?? 0;
  const base = data.base_chunks_at_start ?? null;
  const deregReady = data.deregister_ready === true;
  const notReadyReasons = data.not_ready_reasons ?? [];
  // Physical pool view (US-002 drain-progress-physical). When the backend
  // supports ClusterObjectCountProbe (RADOS), `physical_chunks_on_cluster`
  // is the operator's primary chunk count — the manifest count
  // (`chunks_on_cluster`) drops to 0 as soon as the rebalance worker
  // rewrites BackendRef, but physical chunks linger until STRATA_GC_GRACE
  // elapses + the next gc tick. On backends without the probe the field
  // is null and the manifest count remains primary.
  const physicalChunks =
    data.physical_chunks_on_cluster == null
      ? null
      : (data.physical_chunks_on_cluster as number);
  const physicalBytes =
    data.physical_bytes_on_cluster == null
      ? null
      : (data.physical_bytes_on_cluster as number);
  const gcPending = data.gc_queue_pending ?? 0;
  const primary: number | null = physicalChunks ?? chunks;
  const physicalUnavailable = physicalChunks == null;

  if (isReadonly) {
    return (
      <div
        className="space-y-2 rounded-md border border-orange-500/40 bg-orange-500/5 p-2 text-xs text-orange-800 dark:text-orange-300"
        data-testid="dp-readonly"
      >
        <div className="leading-snug">
          <span className="mr-1 font-semibold uppercase tracking-wide">
            Stop-writes
          </span>
          drain — no migration; reads, deletes, and in-flight multipart
          continue. Undrain to resume writes.
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {onUpgradeToEvacuate && (
            <Button
              type="button"
              size="sm"
              variant="outline"
              className="border-red-500/60 text-red-700 hover:bg-red-500/10 dark:text-red-300"
              onClick={onUpgradeToEvacuate}
              data-testid="dp-upgrade"
            >
              Upgrade to evacuate
            </Button>
          )}
          {onUndrain && (
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={onUndrain}
              disabled={undraining}
              data-testid="dp-undrain"
            >
              {undraining && (
                <Loader2 className="mr-1 h-3 w-3 animate-spin" aria-hidden />
              )}
              Undrain
            </Button>
          )}
        </div>
      </div>
    );
  }

  if (pending) {
    return (
      <div className="space-y-1" data-testid="dp-evacuate">
        <div className="text-[10px] font-semibold uppercase tracking-wide text-red-700 dark:text-red-300">
          Evacuating
        </div>
        <div className="text-xs text-muted-foreground">
          Drain progress: scan pending (rebalance worker hasn't completed a tick)
        </div>
      </div>
    );
  }

  if (deregReady) {
    return (
      <div className="space-y-1" data-testid="dp-evacuate">
        <div className="text-[10px] font-semibold uppercase tracking-wide text-red-700 dark:text-red-300">
          Evacuating
        </div>
        <div
          className="flex items-center gap-2 rounded-md border border-emerald-500/40 bg-emerald-500/5 px-2 py-1 text-xs text-emerald-800 dark:text-emerald-300"
          title="Edit STRATA_RADOS_CLUSTERS env to remove this cluster, then rolling restart. See operator runbook for deregister procedure."
          data-testid="dp-dereg-ready"
        >
          <CheckCircle2 className="h-3.5 w-3.5 shrink-0" aria-hidden />
          <span className="font-medium">✓ Ready to deregister</span>
          <span className="text-emerald-700/70 dark:text-emerald-300/70">
            (env edit + restart)
          </span>
        </div>
      </div>
    );
  }

  // Awaiting GC cleanup (US-002 drain-progress-physical): manifest scan
  // sees 0 chunks but the physical pool still holds chunks pending the
  // STRATA_GC_GRACE window. Render an amber chip with static tooltip
  // describing the wait. Only fires when physical-probe is available
  // (physicalChunks != null) — null-physical backends fall through to
  // the existing not-ready / dereg-ready branches.
  if (chunks === 0 && physicalChunks != null && physicalChunks > 0) {
    // Awaiting GC ETA: gc-config + gc_queue_pending feed the formula
    // (US-002 drain-rebalance-transparency). When the config query is
    // loading/errored, fall back to the static pre-cycle copy so the
    // chip never blocks on a legacy gateway.
    const gcCfg = gcCfgQuery.data;
    const etaSuffix =
      gcCfg != null
        ? ` (${formatEtaMinutesCap(
            computeAwaitingGCEtaMinutes(gcPending, gcCfg),
          )})`
        : '';
    const tooltip =
      gcCfg != null
        ? `ETA computed from current GC queue depth (${gcPending} chunks), STRATA_GC_GRACE (${gcCfg.grace_seconds}s), STRATA_GC_INTERVAL (${gcCfg.interval_seconds}s), STRATA_GC_BATCH_SIZE (${gcCfg.batch_size}), STRATA_GC_SHARDS (${gcCfg.shards}).`
        : AWAITING_GC_TOOLTIP_FALLBACK;
    return (
      <div className="space-y-1" data-testid="dp-evacuate">
        <div className="text-[10px] font-semibold uppercase tracking-wide text-red-700 dark:text-red-300">
          Evacuating
        </div>
        <div
          className="flex items-center gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 px-2 py-1 text-xs text-amber-800 dark:text-amber-300"
          title={tooltip}
          data-testid="dp-awaiting-gc"
        >
          <AlertCircle className="h-3.5 w-3.5 shrink-0" aria-hidden />
          <span className="font-medium">
            Awaiting GC cleanup: {formatCount(physicalChunks)} chunks awaiting
            physical delete{etaSuffix}
          </span>
        </div>
        <DrainDetail
          manifestChunks={chunks}
          gcPending={gcPending}
          physicalBytes={physicalBytes}
          physicalUnavailable={physicalUnavailable}
        />
      </div>
    );
  }

  // chunks=0 but deregister_ready=false → at least one of the auxiliary
  // safety probes (gc_queue_pending / open_multipart) reports a blocker.
  // Surface the unmet reasons as an amber chip so the operator knows why
  // the green dereg chip hasn't flipped (US-006 drain-cleanup).
  if (chunks === 0 && notReadyReasons.length > 0) {
    const reasonsLabel = notReadyReasons
      .map((r) => DRAIN_NOT_READY_REASON_LABELS[r] ?? r)
      .join(', ');
    return (
      <div className="space-y-1" data-testid="dp-evacuate">
        <div className="text-[10px] font-semibold uppercase tracking-wide text-red-700 dark:text-red-300">
          Evacuating
        </div>
        <div
          className="flex items-center gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 px-2 py-1 text-xs text-amber-800 dark:text-amber-300"
          title="Manifest scan reports 0 chunks, but one or more safety probes still report pending work. Deregister is blocked until every probe clears."
          data-testid="dp-not-ready"
        >
          <AlertCircle className="h-3.5 w-3.5 shrink-0" aria-hidden />
          <span className="font-medium">Not ready — {reasonsLabel}</span>
        </div>
      </div>
    );
  }

  if (chunks == null) {
    return null;
  }

  const moved =
    base != null && base > 0 && migratable >= 0
      ? Math.max(0, base - migratable)
      : 0;
  const percent =
    base != null && base > 0
      ? Math.min(100, Math.max(0, (moved / base) * 100))
      : null;

  // Filter the per-bucket entries by category for the drawer drill-down.
  const stuckBuckets = (data.by_bucket ?? []).filter(
    (b: BucketDrainProgressEntry) =>
      b.category === 'stuck_single_policy' ||
      b.category === 'stuck_no_policy',
  );
  const stuckSingleBuckets = stuckBuckets.filter(
    (b) => b.category === 'stuck_single_policy',
  );
  const stuckNoBuckets = stuckBuckets.filter(
    (b) => b.category === 'stuck_no_policy',
  );

  const openStuckDrawer = (
    category: 'stuck_single_policy' | 'stuck_no_policy',
  ) => {
    setDrawerCategory(category);
    setDrawerOpen(true);
  };

  const drawerRows: BucketDrainProgressEntry[] =
    drawerCategory === 'stuck_single_policy'
      ? stuckSingleBuckets
      : drawerCategory === 'stuck_no_policy'
        ? stuckNoBuckets
        : [];

  const drawerTitle =
    drawerCategory === 'stuck_single_policy'
      ? 'Buckets stuck — every policy target is draining'
      : 'Buckets stuck — no placement policy';
  const drawerDescription =
    drawerCategory === 'stuck_single_policy'
      ? 'Each bucket below has a Placement policy whose only targets are draining clusters. Edit Placement to include a live cluster so the rebalance worker can migrate these chunks.'
      : 'Each bucket below has no Placement policy. Class-env routing landed chunks on this draining cluster — set an initial Placement that excludes it.';

  return (
    <div className="space-y-1" data-testid="dp-evacuate">
      <div className="text-[10px] font-semibold uppercase tracking-wide text-red-700 dark:text-red-300">
        Evacuating
      </div>
      {percent != null && (
        <div
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(percent)}
          aria-label={`Drain ${Math.round(percent)}% complete`}
          className="h-1.5 w-full overflow-hidden rounded-full bg-muted"
        >
          <div
            className="h-full bg-amber-500 transition-[width] duration-500 ease-out dark:bg-amber-400"
            style={{ width: `${percent}%` }}
          />
        </div>
      )}
      <div className="grid grid-cols-3 gap-1.5">
        <CategoryCounter
          tone="ok"
          label="Migrating"
          value={migratable}
          testid="dp-count-migratable"
        />
        <CategoryCounter
          tone="stuck"
          label="Stuck (single)"
          value={stuckSingle}
          testid="dp-count-stuck-single"
          onClick={
            stuckSingle > 0 ? () => openStuckDrawer('stuck_single_policy') : undefined
          }
        />
        <CategoryCounter
          tone="stuck"
          label="Stuck (no policy)"
          value={stuckNo}
          testid="dp-count-stuck-no"
          onClick={
            stuckNo > 0 ? () => openStuckDrawer('stuck_no_policy') : undefined
          }
        />
      </div>
      <MigratingSummary
        primary={primary}
        physicalUnavailable={physicalUnavailable}
        base={base}
        migratable={migratable}
        observedBytesPerSec={
          rebProgressQuery.data?.metrics_available
            ? rebProgressQuery.data.observed_bytes_per_sec
            : null
        }
        rebalanceCfg={rebCfgQuery.data ?? null}
        rebalanceCfgError={rebCfgQuery.isError}
        rebalanceProgressError={rebProgressQuery.isError}
        stale={stale}
        fallbackEtaSeconds={data.eta_seconds ?? null}
        clusterID={clusterID}
      />
      <DrainDetail
        manifestChunks={chunks}
        gcPending={gcPending}
        physicalBytes={physicalBytes}
        physicalUnavailable={physicalUnavailable}
      />
      <StuckBucketsDrawer
        open={drawerOpen}
        onOpenChange={setDrawerOpen}
        clusterID={clusterID}
        title={drawerTitle}
        description={drawerDescription}
        rows={drawerRows}
      />
    </div>
  );
}

interface DrainDetailProps {
  manifestChunks: number | null;
  gcPending: number;
  physicalBytes: number | null;
  physicalUnavailable: boolean;
}

// DrainDetail renders the collapsed-by-default detail block that splits
// the primary chunk count into manifest vs physical (US-002
// drain-progress-physical). Operators reach for it when the headline
// reads "Awaiting GC cleanup" — the breakdown shows that manifest=0
// but GC queue is non-empty, so deregister-ready is held only by the
// grace window.
function DrainDetail({
  manifestChunks,
  gcPending,
  physicalBytes,
  physicalUnavailable,
}: DrainDetailProps) {
  return (
    <details
      className="mt-1 rounded-md border border-border/60 bg-muted/30 text-[11px]"
      data-testid="dp-detail"
    >
      <summary className="cursor-pointer select-none px-2 py-1 text-muted-foreground hover:text-foreground">
        Detail
      </summary>
      <dl className="grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 px-2 pb-2 pt-1 tabular-nums">
        <dt className="text-muted-foreground">Manifest chunks</dt>
        <dd
          className="text-foreground"
          data-testid="dp-detail-manifest"
        >
          {manifestChunks == null ? '?' : formatCount(manifestChunks)}
        </dd>
        <dt className="text-muted-foreground">GC queue</dt>
        <dd className="text-foreground" data-testid="dp-detail-gc">
          {formatCount(gcPending)}
        </dd>
        <dt className="text-muted-foreground">Physical bytes</dt>
        <dd
          className={cn(
            physicalUnavailable ? 'italic text-muted-foreground' : 'text-foreground',
          )}
          data-testid="dp-detail-bytes"
        >
          {physicalUnavailable || physicalBytes == null
            ? 'unavailable'
            : formatBytes(physicalBytes)}
        </dd>
      </dl>
    </details>
  );
}

interface MigratingSummaryProps {
  primary: number | null;
  physicalUnavailable: boolean;
  base: number | null;
  migratable: number;
  // observedBytesPerSec is the 1m-window rate from /admin/v1/clusters/.../
  // rebalance-progress. null = metrics unavailable (Prom unset / endpoint
  // 404 or 500 / query still loading) → caller falls back to pre-cycle
  // eta_seconds. >0 = drive Z from observed; 0 = cold start (fallback for
  // ETA, observed row reads cold-start copy).
  observedBytesPerSec: number | null;
  rebalanceCfg: RebalanceConfig | null;
  rebalanceCfgError: boolean;
  rebalanceProgressError: boolean;
  stale: boolean;
  fallbackEtaSeconds: number | null;
  clusterID: string;
}

// MigratingSummary renders the dp-summary row of the Migrating chip.
// Three rendering modes:
//
//   pre-cycle  — rebalance-config OR rebalance-progress query still
//                loading or errored → original chunks-remaining + base +
//                fallback eta_seconds copy
//   live       — both queries resolved → chunks-remaining + observed
//                bandwidth + formula-derived ETA. Tooltip enumerates
//                rate cap + remaining chunks
//   cold-start — live but observed_bytes_per_sec == 0 → observed reads
//                "~0 MB/s observed (cold start)"; ETA uses fallback
//                denominator (rate_mb_s × replicas_count / 2)
function MigratingSummary({
  primary,
  physicalUnavailable,
  base,
  migratable,
  observedBytesPerSec,
  rebalanceCfg,
  rebalanceCfgError,
  rebalanceProgressError,
  stale,
  fallbackEtaSeconds,
  clusterID,
}: MigratingSummaryProps) {
  const queriesReady =
    !rebalanceCfgError &&
    !rebalanceProgressError &&
    rebalanceCfg != null &&
    observedBytesPerSec != null;

  let bandwidthSuffix: JSX.Element | null = null;
  let tooltip: string | undefined;

  if (queriesReady && migratable > 0) {
    const cfg = rebalanceCfg;
    const observed = observedBytesPerSec;
    const observedMBs = observed / (1024 * 1024);
    const etaSeconds = computeMigratingEtaSeconds(migratable, observed, cfg);
    const etaLabel = !Number.isFinite(etaSeconds)
      ? '~24h+ ETA'
      : formatEtaMinutesCap(etaSeconds / 60);
    const observedLabel =
      observed > 0
        ? `~${formatMBs(observedMBs)} MB/s observed`
        : '~0 MB/s observed (cold start)';
    bandwidthSuffix = (
      <>
        {' '}
        · <span data-testid="dp-observed-mbs">{observedLabel}</span> ·{' '}
        <span data-testid="dp-eta-formula">{etaLabel}</span>
      </>
    );
    tooltip = `ETA from observed bandwidth (${formatMBs(observedMBs)} MB/s on cluster ${clusterID}) over remaining manifest chunks (${migratable}). Configured rate cap per replica: ${cfg.rate_mb_s} MB/s.`;
  }

  return (
    <div
      className={cn(
        'tabular-nums text-xs',
        stale ? 'text-muted-foreground/70' : 'text-muted-foreground',
      )}
      data-testid="dp-summary"
      title={tooltip}
    >
      <span className="text-foreground">Migrating: </span>
      <span
        className="font-medium text-foreground"
        data-testid="dp-primary-count"
      >
        {formatCount(primary ?? 0)}
      </span>{' '}
      chunks remaining
      {physicalUnavailable && (
        <span
          className="ml-1 italic"
          title={PHYSICAL_UNAVAILABLE_TOOLTIP}
          data-testid="dp-physical-unavailable"
        >
          (physical count unavailable on this backend)
        </span>
      )}
      {base != null && base > 0 && ` · ${formatCount(base)} at start`}
      {bandwidthSuffix != null
        ? bandwidthSuffix
        : migratable > 0 && fallbackEtaSeconds != null && (
            <> · ~{formatEta(fallbackEtaSeconds)}</>
          )}
      {stale && (
        <span
          className="ml-1 italic"
          title="Cache older than 2×STRATA_REBALANCE_INTERVAL"
        >
          (stale)
        </span>
      )}
    </div>
  );
}

interface CategoryCounterProps {
  tone: 'ok' | 'stuck';
  label: string;
  value: number;
  testid: string;
  onClick?: () => void;
}

function CategoryCounter({
  tone,
  label,
  value,
  testid,
  onClick,
}: CategoryCounterProps) {
  const base = cn(
    'flex flex-col items-start gap-0.5 rounded-md border px-1.5 py-1 text-left tabular-nums transition-colors',
    tone === 'ok'
      ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-800 dark:text-emerald-300'
      : 'border-amber-500/40 bg-amber-500/10 text-amber-800 dark:text-amber-300',
    onClick && 'cursor-pointer hover:brightness-110',
  );
  const inner = (
    <>
      <span className="text-sm font-semibold leading-none">
        {value.toLocaleString()}
      </span>
      <span className="text-[10px] uppercase tracking-wide leading-none">
        {label}
      </span>
    </>
  );
  if (onClick) {
    return (
      <button
        type="button"
        className={base}
        onClick={onClick}
        data-testid={testid}
      >
        {inner}
      </button>
    );
  }
  return (
    <div className={base} data-testid={testid}>
      {inner}
    </div>
  );
}
