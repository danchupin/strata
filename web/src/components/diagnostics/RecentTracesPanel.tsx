import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useSearchParams } from 'react-router-dom';
import { AlertCircle, ArrowDown, X } from 'lucide-react';

import {
  fetchRecentTraces,
  type RecentTracesQuery,
  type TraceSummary,
} from '@/api/client';
import { queryKeys } from '@/lib/query';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';

const RECENT_TRACES_LIMIT = 50;
const RECENT_TRACES_POLL_MS = 10_000;
const PATH_DEBOUNCE_MS = 250;

const METHODS = ['PUT', 'GET', 'DELETE', 'POST', 'HEAD', 'OPTIONS', 'PATCH'] as const;
const STATUSES = ['Error', 'OK'] as const;
const ALL = '__all__';

type MethodValue = '' | (typeof METHODS)[number];
type StatusValue = '' | (typeof STATUSES)[number];
type SortKey = 'started_at' | 'duration_ms';

interface FilterState {
  method: MethodValue;
  status: StatusValue;
  path: string;
  // raw string from <input type=number> so '' = unset and partial typing
  // ('1', '12') round-trips without forcing NaN through state.
  minDurationMs: string;
}

const EMPTY_FILTER: FilterState = {
  method: '',
  status: '',
  path: '',
  minDurationMs: '',
};

// readFilterFromURL parses the four URL params we own. Unknown enum values
// are coerced to '' (no filter) so a stale bookmark with a typo never
// half-applies; the input field always reflects what the URL holds.
export function readFilterFromURL(sp: URLSearchParams): FilterState {
  const m = (sp.get('method') ?? '').toUpperCase();
  const method = (METHODS as readonly string[]).includes(m)
    ? (m as MethodValue)
    : '';
  const s = sp.get('status') ?? '';
  const status = (STATUSES as readonly string[]).includes(s)
    ? (s as StatusValue)
    : '';
  const path = sp.get('path') ?? '';
  const min = sp.get('min_duration_ms') ?? '';
  const minDurationMs = /^\d+$/.test(min) ? min : '';
  return { method, status, path, minDurationMs };
}

// writeFilterToParams returns a new URLSearchParams derived from `sp`
// with the four filter params set (or deleted when empty). Other params
// on the URL are preserved so the panel never clobbers another component's
// state.
export function writeFilterToParams(
  sp: URLSearchParams,
  f: FilterState,
): URLSearchParams {
  const next = new URLSearchParams(sp);
  if (f.method) next.set('method', f.method);
  else next.delete('method');
  if (f.status) next.set('status', f.status);
  else next.delete('status');
  if (f.path) next.set('path', f.path);
  else next.delete('path');
  if (f.minDurationMs) next.set('min_duration_ms', f.minDurationMs);
  else next.delete('min_duration_ms');
  return next;
}

export function isEmptyFilter(f: FilterState): boolean {
  return !f.method && !f.status && !f.path && !f.minDurationMs;
}

export function toRecentTracesQuery(
  f: FilterState,
  debouncedPath: string,
): RecentTracesQuery {
  const q: RecentTracesQuery = {};
  if (f.method) q.method = f.method;
  if (f.status) q.status = f.status;
  if (debouncedPath) q.path = debouncedPath;
  if (f.minDurationMs && /^\d+$/.test(f.minDurationMs)) {
    q.minDurationMs = Number(f.minDurationMs);
  }
  return q;
}

function useDebounced<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}

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
  const [searchParams, setSearchParams] = useSearchParams();

  // `filter` is the *committed* filter (path already debounced). The Select
  // controls + min-duration write to it directly; the path input writes to
  // `pathInput` first and a separate effect commits the debounced value
  // into filter.path. This split lets URL-driven updates (back/forward)
  // apply immediately to both filter AND the input without the 250ms lag.
  const [filter, setFilter] = useState<FilterState>(() =>
    readFilterFromURL(searchParams),
  );
  const [pathInput, setPathInput] = useState(filter.path);
  const [sortKey, setSortKey] = useState<SortKey>('started_at');

  const debouncedPathInput = useDebounced(pathInput, PATH_DEBOUNCE_MS);

  // Commit debounced input into the filter once the operator pauses typing.
  useEffect(() => {
    setFilter((f) =>
      f.path === debouncedPathInput ? f : { ...f, path: debouncedPathInput },
    );
  }, [debouncedPathInput]);

  // Sync filter -> URL on every committed change.
  const lastWrittenRef = useRef<string>('');
  useEffect(() => {
    const next = writeFilterToParams(searchParams, filter);
    const nextStr = next.toString();
    if (nextStr === searchParams.toString()) return;
    if (nextStr === lastWrittenRef.current) return;
    lastWrittenRef.current = nextStr;
    // Push (not replace) so browser back/forward steps through prior
    // filter states. The 250ms path debounce keeps history noise low —
    // a typed-then-paused burst produces one entry, not one per keystroke.
    setSearchParams(next);
    // Deps cover every filter axis. searchParams excluded — the URL ->
    // filter effect handles external nav.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter.method, filter.status, filter.path, filter.minDurationMs]);

  // Sync URL -> filter on external nav (browser back/forward, paste of a
  // shared link). Updates the input field immediately so the operator
  // never sees a stale text value relative to the URL.
  useEffect(() => {
    const fromURL = readFilterFromURL(searchParams);
    setFilter((f) => {
      if (
        fromURL.method === f.method &&
        fromURL.status === f.status &&
        fromURL.path === f.path &&
        fromURL.minDurationMs === f.minDurationMs
      ) {
        return f;
      }
      return fromURL;
    });
    setPathInput((p) => (p === fromURL.path ? p : fromURL.path));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [searchParams]);

  const query = toRecentTracesQuery(filter, filter.path);
  const recentQ = useQuery({
    queryKey: queryKeys.diagnostics.recentTraces(
      RECENT_TRACES_LIMIT,
      0,
      query as Record<string, unknown>,
    ),
    queryFn: () => fetchRecentTraces(RECENT_TRACES_LIMIT, 0, query),
    refetchInterval: RECENT_TRACES_POLL_MS,
    meta: { label: 'recent traces', silent: true },
  });

  const sorted = useMemo<TraceSummary[]>(() => {
    const traces = recentQ.data?.traces ?? [];
    if (sortKey === 'duration_ms') {
      return [...traces].sort((a, b) => b.duration_ms - a.duration_ms);
    }
    return traces;
  }, [recentQ.data, sortKey]);

  const total = recentQ.data?.total ?? 0;
  const isLoading = recentQ.isPending;
  const errorMessage =
    recentQ.error instanceof Error ? recentQ.error.message : null;

  const filterIsEmpty = isEmptyFilter(filter) && !pathInput;
  const showFilteredEmpty =
    !errorMessage && !isLoading && sorted.length === 0 && !filterIsEmpty;
  const showRawEmpty =
    !errorMessage && !isLoading && sorted.length === 0 && filterIsEmpty;

  function handleClear() {
    setFilter(EMPTY_FILTER);
    setPathInput('');
  }

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
        <div
          className="mb-3 grid grid-cols-1 gap-2 sm:grid-cols-12 sm:items-end"
          data-testid="recent-traces-filters"
        >
          <div className="sm:col-span-2">
            <Label htmlFor="rtp-method" className="text-xs">
              Method
            </Label>
            <Select
              value={filter.method || ALL}
              onValueChange={(v) =>
                setFilter((f) => ({
                  ...f,
                  method: v === ALL ? '' : (v as MethodValue),
                }))
              }
            >
              <SelectTrigger
                id="rtp-method"
                className="h-9"
                data-testid="rtp-method"
              >
                <SelectValue placeholder="All" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>All</SelectItem>
                {METHODS.map((m) => (
                  <SelectItem key={m} value={m}>
                    {m}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="sm:col-span-2">
            <Label htmlFor="rtp-status" className="text-xs">
              Status
            </Label>
            <Select
              value={filter.status || ALL}
              onValueChange={(v) =>
                setFilter((f) => ({
                  ...f,
                  status: v === ALL ? '' : (v as StatusValue),
                }))
              }
            >
              <SelectTrigger
                id="rtp-status"
                className="h-9"
                data-testid="rtp-status"
              >
                <SelectValue placeholder="All" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>All</SelectItem>
                {STATUSES.map((s) => (
                  <SelectItem key={s} value={s}>
                    {s}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="sm:col-span-5">
            <Label htmlFor="rtp-path" className="text-xs">
              Path substring
            </Label>
            <Input
              id="rtp-path"
              type="search"
              value={pathInput}
              onChange={(e) => setPathInput(e.target.value)}
              placeholder="Filter by path (e.g. demo-cephb)"
              className="h-9"
              data-testid="rtp-path"
              maxLength={256}
            />
          </div>
          <div className="sm:col-span-2">
            <Label htmlFor="rtp-min" className="text-xs">
              Min duration ms
            </Label>
            <Input
              id="rtp-min"
              type="number"
              inputMode="numeric"
              min={0}
              value={filter.minDurationMs}
              onChange={(e) => {
                const v = e.target.value;
                // Reject negatives; allow '' for clearing.
                if (v === '' || /^\d+$/.test(v)) {
                  setFilter((f) => ({ ...f, minDurationMs: v }));
                }
              }}
              placeholder="0"
              className="h-9"
              data-testid="rtp-min-duration"
            />
          </div>
          <div className="sm:col-span-1 sm:flex sm:justify-end">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleClear}
              disabled={filterIsEmpty}
              data-testid="rtp-clear"
              className="h-9 w-full sm:w-auto"
            >
              <X className="mr-1 h-3.5 w-3.5" aria-hidden /> Clear
            </Button>
          </div>
        </div>

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
        {showRawEmpty && (
          <div className="flex flex-col items-center justify-center gap-1 py-8 text-center text-sm text-muted-foreground">
            <div>No traces captured yet.</div>
            <div className="text-xs">
              Send a request to populate the ringbuf.
            </div>
          </div>
        )}
        {showFilteredEmpty && (
          <div
            className="flex flex-col items-center justify-center gap-1 py-8 text-center text-sm text-muted-foreground"
            data-testid="recent-traces-empty-filtered"
          >
            <div>No traces match the current filters.</div>
            <div className="text-xs">Widen filters or click Clear.</div>
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
