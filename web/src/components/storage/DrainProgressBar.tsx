import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { CheckCircle2 } from 'lucide-react';

import { fetchClusterDrainProgress } from '@/api/client';
import { queryKeys } from '@/lib/query';
import { cn } from '@/lib/utils';

const DRAIN_PROGRESS_POLL_MS = 30_000;

interface Props {
  clusterID: string;
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
// each draining cluster card (US-004 drain-lifecycle). Reads from
// GET /admin/v1/clusters/{id}/drain-progress every 30s via the shared
// query key. Three render shapes:
//   - chunks > 0 + base known → progress bar + "<X> chunks · <Y> remaining · ~<ETA>"
//   - chunks > 0 + base unknown → text only (no bar)
//   - chunks == 0 + deregister_ready → green chip "Ready to deregister"
//
// The "(stale)" suffix + muted text fire when the response includes the
// "progress data stale" warning (cache older than 2×STRATA_REBALANCE_INTERVAL).
export function DrainProgressBar({ clusterID }: Props) {
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

  const stale = (data.warnings ?? []).includes('progress data stale');
  const pending = (data.warnings ?? []).some((w) =>
    w.startsWith('progress scan pending'),
  );
  const chunks = data.chunks_on_cluster ?? null;
  const base = data.base_chunks_at_start ?? null;
  const deregReady = data.deregister_ready === true;

  if (pending) {
    return (
      <div className="text-xs text-muted-foreground">
        Drain progress: scan pending (rebalance worker hasn't completed a tick)
      </div>
    );
  }

  if (deregReady) {
    return (
      <div
        className="flex items-center gap-2 rounded-md border border-emerald-500/40 bg-emerald-500/5 px-2 py-1 text-xs text-emerald-800 dark:text-emerald-300"
        title="Drain complete. Edit STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS to remove this id, then rolling-restart the gateway to deregister."
      >
        <CheckCircle2 className="h-3.5 w-3.5 shrink-0" aria-hidden />
        <span className="font-medium">Ready to deregister</span>
        <span className="text-emerald-700/70 dark:text-emerald-300/70">
          (env edit + restart)
        </span>
      </div>
    );
  }

  if (chunks == null) {
    return null;
  }

  const moved =
    base != null && base > 0 && chunks >= 0 ? Math.max(0, base - chunks) : 0;
  const percent =
    base != null && base > 0
      ? Math.min(100, Math.max(0, (moved / base) * 100))
      : null;

  return (
    <div className="space-y-1">
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
      <div
        className={cn(
          'tabular-nums text-xs',
          stale ? 'text-muted-foreground/70' : 'text-muted-foreground',
        )}
      >
        <span className="font-medium text-foreground">
          {formatCount(chunks)}
        </span>{' '}
        chunks remaining
        {base != null && base > 0 && ` · ${formatCount(base)} at start`}
        {data.eta_seconds != null && ` · ~${formatEta(data.eta_seconds)}`}
        {stale && (
          <span className="ml-1 italic" title="Cache older than 2×STRATA_REBALANCE_INTERVAL">
            (stale)
          </span>
        )}
      </div>
    </div>
  );
}
