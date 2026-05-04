import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useVirtualizer } from '@tanstack/react-virtual';
import { AlertCircle, Loader2, Pause, Play, Trash2 } from 'lucide-react';

import {
  fetchBucketsList,
  fetchIAMUsers,
  type AuditRecord,
} from '@/api/client';
import { AuditEventDetailSheet } from '@/components/AuditEventDetailSheet';
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
import { useEventSource } from '@/hooks/useEventSource';
import { cn } from '@/lib/utils';

// RING_CAPACITY caps the in-memory event ring. 10k rows comfortably stay in
// the 1–2 MiB band (≈150 B/row JSON) and the virtualiser keeps the DOM at
// ~40 rendered rows regardless of buffer depth.
const RING_CAPACITY = 10_000;
const FILTER_DEBOUNCE_MS = 300;

// Action choices intentionally mirror the historical viewer (US-018) so the
// surface stays consistent and operators recognise the verbs in either page.
const ACTION_OPTIONS: string[] = [
  'PutObject',
  'DeleteObject',
  'PutObjectTagging',
  'PutObjectRetention',
  'PutObjectLegalHold',
  'CreateMultipartUpload',
  'UploadPart',
  'CompleteMultipartUpload',
  'AbortMultipartUpload',
  'PutBucket',
  'DeleteBucket',
  'PutBucketLifecycle',
  'PutBucketCORS',
  'DeleteBucketCORS',
  'PutBucketPolicy',
  'DeleteBucketPolicy',
  'PutBucketAcl',
  'PutBucketVersioning',
  'PutBucketLogging',
  'DeleteBucketLogging',
  'PutBucketInventory',
  'DeleteBucketInventory',
  'admin:CreateBucket',
  'admin:DeleteBucket',
  'admin:CreateIAMUser',
  'admin:DeleteIAMUser',
  'admin:CreateAccessKey',
  'admin:DeleteAccessKey',
];

function useDebounced<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}

// formatRelative returns "<n>s ago" / "<n>m ago" / RFC-ish ISO. Keeps the
// row narrow without the table re-rendering on every tick — the relative
// time is recomputed only when an event is added or the filter changes.
function formatRelative(ts: string, now: number): string {
  const t = new Date(ts).getTime();
  if (Number.isNaN(t)) return ts;
  const diff = Math.max(0, now - t);
  const secs = Math.floor(diff / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  return `${hours}h ago`;
}

export function AuditTailPage() {
  const [actionsSelected, setActionsSelected] = useState<Set<string>>(new Set());
  const [actionsPickerOpen, setActionsPickerOpen] = useState(false);
  const [principal, setPrincipal] = useState('');
  const [bucket, setBucket] = useState('');
  const [paused, setPaused] = useState(false);
  // events is the visible, append-ordered ring (newest last).
  const [events, setEvents] = useState<AuditRecord[]>([]);
  // skippedWhilePaused is the count of frames discarded while paused, shown
  // in the resume banner so operators know how much they missed.
  const [skippedWhilePaused, setSkippedWhilePaused] = useState(0);
  // resumedSkipBanner is the carry-over message rendered after Resume so the
  // operator sees the skipped count for one banner-life-time.
  const [resumedSkipBanner, setResumedSkipBanner] = useState<number | null>(null);
  const [streamedTotal, setStreamedTotal] = useState(0);
  const [detailRecord, setDetailRecord] = useState<AuditRecord | null>(null);
  const [now, setNow] = useState(() => Date.now());

  const debouncedPrincipal = useDebounced(principal.trim(), FILTER_DEBOUNCE_MS);
  const debouncedBucket = useDebounced(bucket.trim(), FILTER_DEBOUNCE_MS);
  const actionParam = useMemo(
    () => Array.from(actionsSelected).sort().join(','),
    [actionsSelected],
  );

  // Rebuild EventSource URL whenever the server-side filter changes. Live
  // tail is server-filtered (broadcaster-side `Filter`), so a wire-level
  // re-subscribe is the right way to apply a new filter.
  const streamURL = useMemo(() => {
    const usp = new URLSearchParams();
    if (actionParam) usp.set('action', actionParam);
    if (debouncedPrincipal) usp.set('principal', debouncedPrincipal);
    if (debouncedBucket) usp.set('bucket', debouncedBucket);
    const qs = usp.toString();
    return `/admin/v1/audit/stream${qs ? `?${qs}` : ''}`;
  }, [actionParam, debouncedPrincipal, debouncedBucket]);

  const bucketsQ = useQuery({
    queryKey: ['buckets', 'list', { query: '', sort: '', order: 'asc', page: 1, pageSize: 1000 }],
    queryFn: () => fetchBucketsList({ pageSize: 1000 }),
    meta: { silent: true },
  });
  const usersQ = useQuery({
    queryKey: ['iam', 'users', { query: '', page: 1, pageSize: 500 }],
    queryFn: () => fetchIAMUsers({ pageSize: 500 }),
    meta: { silent: true },
  });
  const bucketSuggestions = useMemo(
    () => (bucketsQ.data?.buckets ?? []).map((b) => b.name).sort(),
    [bucketsQ.data],
  );
  const principalSuggestions = useMemo(
    () => (usersQ.data?.users ?? []).map((u) => u.user_name).sort(),
    [usersQ.data],
  );

  // pausedRef carries the latest paused flag into the EventSource onMessage
  // closure without re-subscribing — the connection survives Pause/Resume.
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  const { state, retryInSeconds } = useEventSource({
    url: streamURL,
    onMessage: (raw) => {
      let parsed: AuditRecord;
      try {
        parsed = JSON.parse(raw) as AuditRecord;
      } catch {
        return;
      }
      if (pausedRef.current) {
        setSkippedWhilePaused((n) => n + 1);
        return;
      }
      setStreamedTotal((n) => n + 1);
      setEvents((prev) => {
        const next = prev.length >= RING_CAPACITY ? prev.slice(1) : prev.slice();
        next.push(parsed);
        return next;
      });
    },
  });

  // Tick a one-second clock so the relative-time labels update live.
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  function handlePause() {
    setPaused(true);
    setSkippedWhilePaused(0);
  }

  function handleResume() {
    setPaused(false);
    if (skippedWhilePaused > 0) setResumedSkipBanner(skippedWhilePaused);
    setSkippedWhilePaused(0);
  }

  function handleClear() {
    setEvents([]);
    setStreamedTotal(0);
  }

  function toggleAction(name: string) {
    setActionsSelected((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  }

  function handleClearActions() {
    setActionsSelected(new Set());
  }

  // Newest-first display order; virtualiser indexes 0..events.length-1.
  const displayEvents = useMemo(() => events.slice().reverse(), [events]);

  const scrollRef = useRef<HTMLDivElement | null>(null);
  const virtualizer = useVirtualizer({
    count: displayEvents.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 40,
    overscan: 12,
  });

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Audit tail</h1>
        <p className="text-sm text-muted-foreground">
          Live stream of state-changing events. Subscribes to{' '}
          <code className="font-mono text-xs">/admin/v1/audit/stream</code>;
          server-side filters apply at the broadcaster.
        </p>
      </div>

      {state === 'reconnecting' && (
        <Card className="border-amber-500/40 bg-amber-500/5">
          <CardContent className="flex items-start gap-2 py-3 text-sm">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-amber-600" aria-hidden />
            <span>
              Connection lost — reconnecting in {retryInSeconds}s
            </span>
          </CardContent>
        </Card>
      )}
      {resumedSkipBanner != null && resumedSkipBanner > 0 && (
        <Card className="border-muted bg-muted/30">
          <CardContent className="flex items-start justify-between gap-2 py-3 text-sm">
            <span>{resumedSkipBanner} events skipped while paused</span>
            <button
              type="button"
              className="text-xs underline-offset-2 hover:underline"
              onClick={() => setResumedSkipBanner(null)}
            >
              dismiss
            </button>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Filters</CardTitle>
          <CardDescription>
            Filters re-subscribe the stream server-side; the buffered ring is preserved.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          <div>
            <Label className="text-xs" htmlFor="audit-tail-principal">
              Principal
            </Label>
            <Input
              id="audit-tail-principal"
              value={principal}
              onChange={(e) => setPrincipal(e.target.value)}
              placeholder="any"
              list="audit-tail-principal-suggestions"
            />
            <datalist id="audit-tail-principal-suggestions">
              {principalSuggestions.map((p) => (
                <option key={p} value={p} />
              ))}
            </datalist>
          </div>
          <div>
            <Label className="text-xs" htmlFor="audit-tail-bucket">
              Bucket
            </Label>
            <Input
              id="audit-tail-bucket"
              value={bucket}
              onChange={(e) => setBucket(e.target.value)}
              placeholder="any"
              list="audit-tail-bucket-suggestions"
            />
            <datalist id="audit-tail-bucket-suggestions">
              {bucketSuggestions.map((b) => (
                <option key={b} value={b} />
              ))}
            </datalist>
          </div>
          <div>
            <Label className="text-xs">Action</Label>
            <div className="flex items-center gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setActionsPickerOpen((v) => !v)}
                aria-expanded={actionsPickerOpen}
              >
                {actionsSelected.size === 0
                  ? 'Any action'
                  : `${actionsSelected.size} selected`}
              </Button>
              {actionsSelected.size > 0 && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={handleClearActions}
                  className="text-xs"
                >
                  Clear
                </Button>
              )}
            </div>
            {actionsPickerOpen && (
              <div className="mt-2 max-h-56 overflow-y-auto rounded-md border bg-popover p-2 text-xs shadow-sm">
                {ACTION_OPTIONS.map((a) => {
                  const checked = actionsSelected.has(a);
                  return (
                    <label
                      key={a}
                      className="flex cursor-pointer items-center gap-2 px-1 py-0.5 hover:bg-accent/50"
                    >
                      <input
                        type="checkbox"
                        checked={checked}
                        onChange={() => toggleAction(a)}
                        className="h-3.5 w-3.5"
                      />
                      <span className="font-mono">{a}</span>
                    </label>
                  );
                })}
              </div>
            )}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Live events</CardTitle>
            <CardDescription className="flex items-center gap-2">
              <span>{streamedTotal} events streamed</span>
              <span aria-hidden>·</span>
              <span className={cn('inline-flex items-center gap-1', stateColor(state))}>
                {(state === 'connecting' || state === 'reconnecting') && (
                  <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
                )}
                {stateLabel(state, paused)}
              </span>
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            {paused ? (
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={handleResume}
              >
                <Play className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                Resume
              </Button>
            ) : (
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={handlePause}
              >
                <Pause className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                Pause
              </Button>
            )}
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleClear}
              disabled={events.length === 0}
            >
              <Trash2 className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              Clear
            </Button>
          </div>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          <div className="grid grid-cols-[10rem_14rem_12rem_1fr_6rem] gap-2 border-y bg-muted/30 px-4 py-2 text-xs font-medium uppercase tracking-wide text-muted-foreground sm:px-6">
            <div>Time</div>
            <div>Action</div>
            <div>Principal</div>
            <div>Resource</div>
            <div className="text-right">Result</div>
          </div>
          <div
            ref={scrollRef}
            className="h-[480px] overflow-y-auto"
            data-testid="audit-tail-list"
          >
            {displayEvents.length === 0 ? (
              <div className="flex h-full flex-col items-center justify-center gap-1 text-sm text-muted-foreground">
                <div>{paused ? 'Paused — buffer empty.' : 'Waiting for events…'}</div>
                <div className="text-xs">
                  Issue a state-changing request (PUT/POST/DELETE) to populate the tail.
                </div>
              </div>
            ) : (
              <div
                style={{ height: virtualizer.getTotalSize(), position: 'relative' }}
              >
                {virtualizer.getVirtualItems().map((vi) => {
                  const r = displayEvents[vi.index];
                  if (!r) return null;
                  return (
                    <div
                      key={`${r.event_id}-${vi.index}`}
                      className="absolute left-0 right-0 grid cursor-pointer grid-cols-[10rem_14rem_12rem_1fr_6rem] gap-2 border-b px-4 py-2 text-xs hover:bg-accent/40 sm:px-6"
                      style={{
                        transform: `translateY(${vi.start}px)`,
                      }}
                      onClick={() => setDetailRecord(r)}
                    >
                      <div className="truncate" title={r.time}>
                        {formatRelative(r.time, now)}
                      </div>
                      <div className="truncate font-mono">{r.action}</div>
                      <div className="truncate font-mono">
                        {r.principal || '—'}
                      </div>
                      <div className="truncate font-mono" title={r.resource}>
                        {r.resource}
                      </div>
                      <div className="truncate text-right tabular-nums">
                        {r.result || '—'}
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        </CardContent>
      </Card>

      <AuditEventDetailSheet
        record={detailRecord}
        open={detailRecord !== null}
        onOpenChange={(o) => {
          if (!o) setDetailRecord(null);
        }}
      />
    </div>
  );
}

function stateLabel(state: string, paused: boolean): string {
  if (paused) return 'paused';
  switch (state) {
    case 'open':
      return 'connected';
    case 'connecting':
      return 'connecting';
    case 'reconnecting':
      return 'reconnecting';
    default:
      return 'closed';
  }
}

function stateColor(state: string): string {
  switch (state) {
    case 'open':
      return 'text-emerald-600 dark:text-emerald-400';
    case 'reconnecting':
      return 'text-amber-600 dark:text-amber-400';
    case 'connecting':
      return 'text-muted-foreground';
    default:
      return 'text-muted-foreground';
  }
}
