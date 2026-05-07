import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertTriangle, X } from 'lucide-react';
import { Link } from 'react-router-dom';

import { fetchStorageHealth, type StorageHealthResponse } from '@/api/client';
import { Button } from '@/components/ui/button';
import { queryKeys } from '@/lib/query';
import { cn } from '@/lib/utils';

const POLL_MS = 30_000;
const DISMISS_KEY = 'strata.storage-degraded.dismissed';

function readDismissedSet(): Set<string> {
  if (typeof window === 'undefined') return new Set();
  try {
    const raw = window.sessionStorage.getItem(DISMISS_KEY);
    if (!raw) return new Set();
    const arr = JSON.parse(raw);
    return Array.isArray(arr) ? new Set(arr.map(String)) : new Set();
  } catch {
    return new Set();
  }
}

function persistDismissedSet(s: Set<string>) {
  if (typeof window === 'undefined') return;
  try {
    window.sessionStorage.setItem(DISMISS_KEY, JSON.stringify([...s]));
  } catch {
    // sessionStorage may throw under privacy modes — silently degrade so the
    // banner just keeps showing rather than crashing the shell.
  }
}

// signatureForHealth folds the current degraded payload into a stable string
// so the dismiss flag is keyed to "this set of warnings". A fresh / different
// degraded condition reappears even if a prior one was dismissed.
function signatureForHealth(h: StorageHealthResponse): string {
  if (h.ok) return 'ok';
  return `${h.source ?? ''}|${[...h.warnings].sort().join('||')}`;
}

export function StorageDegradedBanner() {
  const q = useQuery({
    queryKey: queryKeys.storage.health,
    queryFn: fetchStorageHealth,
    refetchInterval: POLL_MS,
    meta: { label: 'storage health', silent: true },
  });

  const [dismissed, setDismissed] = useState<Set<string>>(() => readDismissedSet());

  useEffect(() => {
    persistDismissedSet(dismissed);
  }, [dismissed]);

  const sig = useMemo(() => (q.data ? signatureForHealth(q.data) : 'ok'), [q.data]);

  if (!q.data || q.data.ok) return null;
  if (dismissed.has(sig)) return null;

  // Worst 3 warnings — backends can emit dozens; the banner is a teaser, the
  // /storage page renders the full list.
  const top = q.data.warnings.slice(0, 3);
  const more = q.data.warnings.length - top.length;

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
            Storage degraded
            {q.data.source ? ` (${q.data.source} backend)` : ''}
          </div>
          {top.length > 0 && (
            <ul className="list-inside list-disc space-y-0.5 text-xs">
              {top.map((w, i) => (
                <li key={`${i}-${w}`}>{w}</li>
              ))}
              {more > 0 && (
                <li className="list-none text-amber-900/80 dark:text-amber-200/80">
                  +{more} more — see Storage page
                </li>
              )}
            </ul>
          )}
          <div className="text-xs">
            <Link
              to="/storage"
              className="font-medium underline underline-offset-2"
            >
              View Storage page
            </Link>
          </div>
        </div>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 shrink-0 text-amber-900 hover:bg-amber-500/20 dark:text-amber-100"
          aria-label="Dismiss storage degraded banner"
          onClick={() => {
            setDismissed((prev) => {
              const next = new Set(prev);
              next.add(sig);
              return next;
            });
          }}
        >
          <X className="h-4 w-4" />
        </Button>
      </div>
    </div>
  );
}
