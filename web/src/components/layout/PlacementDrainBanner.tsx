import { useEffect, useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertTriangle, X } from 'lucide-react';
import { Link, useLocation } from 'react-router-dom';

import { fetchClusters } from '@/api/client';
import { Button } from '@/components/ui/button';
import { queryKeys } from '@/lib/query';
import { cn } from '@/lib/utils';

import {
  readDismissedStamp,
  shouldHideBanner,
  stampForDrainingIds,
  writeDismissedStamp,
} from './placement-drain-banner-dismissal';

const POLL_MS = 15_000;

// PlacementDrainBanner renders a cluster-wide reminder banner whenever
// any registered cluster sits in state=draining. Polls the shared
// `clusters` query key alongside ClustersSubsection so the two
// consumers share one fetch per cache window. The banner is
// session-dismissible — the dismissal stamp is keyed to the SET of
// draining ids, so a new cluster entering the draining set
// re-surfaces the banner even after a prior dismiss.
export function PlacementDrainBanner() {
  const location = useLocation();
  const q = useQuery({
    queryKey: queryKeys.clusters,
    queryFn: fetchClusters,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'clusters', silent: true },
  });

  const [storedStamp, setStoredStamp] = useState<string | null>(() =>
    readDismissedStamp(),
  );

  const drainingIds = useMemo(() => {
    const ids = (q.data?.clusters ?? [])
      .filter((c) => c.state?.toLowerCase() === 'draining')
      .map((c) => c.id);
    ids.sort();
    return ids;
  }, [q.data]);

  // Re-read on route changes so multi-tab dismisses propagate after
  // the next navigation. localStorage 'storage' events would catch
  // cross-tab, but per-session the AC scope is single-tab.
  useEffect(() => {
    setStoredStamp(readDismissedStamp());
  }, [location.pathname]);

  if (location.pathname.startsWith('/login')) return null;
  if (drainingIds.length === 0) return null;
  if (shouldHideBanner(drainingIds, storedStamp)) return null;

  const handleDismiss = () => {
    const stamp = stampForDrainingIds(drainingIds);
    writeDismissedStamp(stamp);
    setStoredStamp(stamp);
  };

  return (
    <div
      role="alert"
      className={cn(
        'border-b border-amber-500/40 bg-amber-500/10 text-amber-950 dark:text-amber-100',
      )}
    >
      <div className="flex items-start gap-3 px-4 py-2 text-sm sm:px-6">
        <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
        <div className="flex-1 space-y-1">
          <div className="font-medium">
            Draining cluster(s): {drainingIds.join(', ')}.
          </div>
          <div className="text-xs">
            Rebalance worker is moving chunks off them.{' '}
            <Link
              to="/storage#clusters"
              className="font-medium underline underline-offset-2"
            >
              View details →
            </Link>
          </div>
        </div>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 shrink-0 text-amber-900 hover:bg-amber-500/20 dark:text-amber-100"
          aria-label="Dismiss draining-cluster banner"
          onClick={handleDismiss}
        >
          <X className="h-4 w-4" />
        </Button>
      </div>
    </div>
  );
}
