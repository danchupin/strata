import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, ArrowDown } from 'lucide-react';

import { fetchRecentTraces, type TraceSummary } from '@/api/client';
import { queryKeys } from '@/lib/query';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';

const RECENT_TRACES_LIMIT = 50;
const RECENT_TRACES_POLL_MS = 10_000;

type SortKey = 'started_at' | 'duration_ms';

function formatDurationMs(ms: number): string {
  if (ms <= 0) return '0 ms';
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

function formatStarted(ns: number): string {
  if (!ns) return '—';
  const ms = Math.round(ns / 1_000_000);
  const d = new Date(ms);
  return d.toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

function shortLabel(s: string, max = 24): string {
  if (s.length <= max) return s;
  return `${s.slice(0, 8)}…${s.slice(-6)}`;
}

interface RecentTracesPanelProps {
  // onSelect navigates the parent to /diagnostics/trace/<id>. Prefer
  // request_id when present so the operator-facing URL stays stable; fall
  // back to trace_id otherwise.
  onSelect: (idOrRequestID: string) => void;
}

export function RecentTracesPanel({ onSelect }: RecentTracesPanelProps) {
  const [sortKey, setSortKey] = useState<SortKey>('started_at');
  const recentQ = useQuery({
    queryKey: queryKeys.diagnostics.recentTraces(RECENT_TRACES_LIMIT, 0),
    queryFn: () => fetchRecentTraces(RECENT_TRACES_LIMIT, 0),
    refetchInterval: RECENT_TRACES_POLL_MS,
    meta: { label: 'recent traces', silent: true },
  });

  const sorted = useMemo<TraceSummary[]>(() => {
    const traces = recentQ.data?.traces ?? [];
    if (sortKey === 'duration_ms') {
      return [...traces].sort((a, b) => b.duration_ms - a.duration_ms);
    }
    // Server returns LRU front first → already started_at desc.
    return traces;
  }, [recentQ.data, sortKey]);

  const total = recentQ.data?.total ?? 0;
  const isLoading = recentQ.isPending;
  const errorMessage =
    recentQ.error instanceof Error ? recentQ.error.message : null;

  return (
    <Card data-testid="recent-traces-panel">
      <CardHeader className="flex flex-row items-start justify-between gap-2 space-y-0">
        <div>
          <CardTitle className="text-base">Recent traces</CardTitle>
          <CardDescription>
            Last {RECENT_TRACES_LIMIT} captured by the in-process ring buffer
            ({total} retained). Refreshes every {RECENT_TRACES_POLL_MS / 1000}s.
          </CardDescription>
        </div>
        <div
          className="inline-flex items-center gap-1 rounded-md border border-input bg-background p-0.5 text-xs"
          role="group"
          aria-label="Sort recent traces"
        >
          <SortButton
            label="Started"
            active={sortKey === 'started_at'}
            onClick={() => setSortKey('started_at')}
          />
          <SortButton
            label="Duration"
            active={sortKey === 'duration_ms'}
            onClick={() => setSortKey('duration_ms')}
          />
        </div>
      </CardHeader>
      <CardContent>
        {errorMessage && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load recent traces</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </div>
        )}
        {!errorMessage && isLoading && (
          <div className="space-y-2">
            <Skeleton className="h-6 w-full" />
            <Skeleton className="h-6 w-full" />
            <Skeleton className="h-6 w-3/4" />
          </div>
        )}
        {!errorMessage && !isLoading && sorted.length === 0 && (
          <div className="flex flex-col items-center justify-center gap-1 py-8 text-center text-sm text-muted-foreground">
            <div>No traces captured yet.</div>
            <div className="text-xs">
              Send a request to populate the ringbuf.
            </div>
          </div>
        )}
        {!errorMessage && !isLoading && sorted.length > 0 && (
          <ul className="divide-y divide-border" role="list">
            {sorted.map((t) => {
              const id = t.request_id || t.trace_id;
              const isError = t.status === 'Error';
              return (
                <li key={`${t.trace_id}-${t.started_at_ns}`}>
                  <button
                    type="button"
                    onClick={() => onSelect(id)}
                    data-testid="recent-trace-row"
                    className="flex w-full items-center gap-3 px-1 py-1.5 text-left transition-colors hover:bg-muted/40"
                  >
                    <span
                      className={cn(
                        'inline-flex h-2.5 w-2.5 shrink-0 rounded-full',
                        isError ? 'bg-red-500' : 'bg-emerald-500',
                      )}
                      aria-hidden
                      title={t.status}
                    />
                    <span className="flex-1 truncate font-mono text-xs">
                      {t.root_name || '(unnamed root)'}
                    </span>
                    <span className="hidden w-44 truncate font-mono text-xs text-muted-foreground sm:inline">
                      {shortLabel(id)}
                    </span>
                    <span className="w-20 shrink-0 text-right font-mono text-xs tabular-nums text-muted-foreground">
                      {formatDurationMs(t.duration_ms)}
                    </span>
                    <span className="w-20 shrink-0 text-right font-mono text-xs tabular-nums text-muted-foreground">
                      {formatStarted(t.started_at_ns)}
                    </span>
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

interface SortButtonProps {
  label: string;
  active: boolean;
  onClick: () => void;
}

function SortButton({ label, active, onClick }: SortButtonProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        'inline-flex items-center gap-1 rounded px-2 py-1 transition-colors',
        active ? 'bg-muted font-medium' : 'text-muted-foreground hover:bg-muted/60',
      )}
    >
      {label}
      {active && <ArrowDown className="h-3 w-3" aria-hidden />}
    </button>
  );
}
