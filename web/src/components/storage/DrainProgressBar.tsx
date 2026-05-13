import { useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { CheckCircle2, Loader2 } from 'lucide-react';

import {
  fetchClusterDrainProgress,
  type BucketDrainProgressEntry,
} from '@/api/client';
import { Button } from '@/components/ui/button';
import { queryKeys } from '@/lib/query';
import { cn } from '@/lib/utils';

import { StuckBucketsDrawer } from './StuckBucketsDrawer';

const DRAIN_PROGRESS_POLL_MS = 30_000;

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
          title="Drain complete. Edit STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS to remove this id, then rolling-restart the gateway to deregister."
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
      <div
        className={cn(
          'tabular-nums text-xs',
          stale ? 'text-muted-foreground/70' : 'text-muted-foreground',
        )}
        data-testid="dp-summary"
      >
        <span className="font-medium text-foreground">
          {formatCount(chunks)}
        </span>{' '}
        total
        {base != null && base > 0 && ` · ${formatCount(base)} at start`}
        {migratable > 0 && data.eta_seconds != null && (
          <> · ~{formatEta(data.eta_seconds)}</>
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
