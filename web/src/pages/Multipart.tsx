import { useEffect, useMemo, useState } from 'react';
import { keepPreviousData, useMutation, useQuery } from '@tanstack/react-query';
import { AlertCircle, RefreshCw, X } from 'lucide-react';

import {
  abortMultipartBatch,
  fetchBucketsList,
  fetchMultipartActive,
  type MultipartActiveRow,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
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
import { Skeleton } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { cn } from '@/lib/utils';

const PAGE_SIZE = 50;
const POLL_INTERVAL_MS = 30_000;
const FILTER_DEBOUNCE_MS = 300;

function useDebounced<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}

function rowKey(r: MultipartActiveRow): string {
  return `${r.bucket}\x00${r.upload_id}`;
}

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function formatAge(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—';
  const sec = Math.floor(seconds);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h`;
  const day = Math.floor(hr / 24);
  return `${day}d`;
}

export function MultipartPage() {
  const [bucket, setBucket] = useState('');
  const [minAgeHoursRaw, setMinAgeHoursRaw] = useState('24');
  const [initiator, setInitiator] = useState('');
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const debouncedBucket = useDebounced(bucket.trim(), FILTER_DEBOUNCE_MS);
  const debouncedInitiator = useDebounced(initiator.trim(), FILTER_DEBOUNCE_MS);

  const minAgeHours = useMemo(() => {
    const n = parseInt(minAgeHoursRaw, 10);
    if (!Number.isFinite(n) || n < 0) return 0;
    return n;
  }, [minAgeHoursRaw]);

  useEffect(() => {
    setPage(1);
  }, [debouncedBucket, debouncedInitiator, minAgeHours]);

  const bucketsQ = useQuery({
    queryKey: ['buckets', 'list', { query: '', sort: '', order: 'asc', page: 1, pageSize: 1000 }],
    queryFn: () => fetchBucketsList({ pageSize: 1000 }),
    meta: { silent: true },
  });
  const bucketSuggestions = useMemo(
    () => (bucketsQ.data?.buckets ?? []).map((b) => b.name).sort(),
    [bucketsQ.data],
  );

  const q = useQuery({
    queryKey: queryKeys.multipart.active(debouncedBucket, minAgeHours, debouncedInitiator, page, PAGE_SIZE),
    queryFn: () =>
      fetchMultipartActive({
        bucket: debouncedBucket || undefined,
        minAgeHours,
        initiator: debouncedInitiator || undefined,
        page,
        pageSize: PAGE_SIZE,
      }),
    refetchInterval: POLL_INTERVAL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'multipart uploads' },
  });

  const rows = q.data?.uploads ?? [];
  const total = q.data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const showSkeleton = q.isPending && !q.data;
  const errorMessage = !q.data && q.error instanceof Error ? q.error.message : null;

  // Drop selections that are no longer in the visible page.
  useEffect(() => {
    if (selected.size === 0) return;
    const visible = new Set(rows.map(rowKey));
    let changed = false;
    const next = new Set<string>();
    for (const k of selected) {
      if (visible.has(k)) {
        next.add(k);
      } else {
        changed = true;
      }
    }
    if (changed) setSelected(next);
  }, [rows, selected]);

  const allVisibleSelected = rows.length > 0 && rows.every((r) => selected.has(rowKey(r)));
  const someVisibleSelected = rows.some((r) => selected.has(rowKey(r)));

  function toggleAll(checked: boolean) {
    if (checked) {
      const next = new Set(selected);
      for (const r of rows) next.add(rowKey(r));
      setSelected(next);
    } else {
      const next = new Set(selected);
      for (const r of rows) next.delete(rowKey(r));
      setSelected(next);
    }
  }

  function toggleOne(r: MultipartActiveRow, checked: boolean) {
    setSelected((prev) => {
      const next = new Set(prev);
      const k = rowKey(r);
      if (checked) next.add(k);
      else next.delete(k);
      return next;
    });
  }

  const abortM = useMutation({
    mutationFn: (targets: { bucket: string; upload_id: string }[]) =>
      abortMultipartBatch(targets),
    onSuccess: (resp) => {
      const aborted = resp.results.filter((r) => r.status === 'aborted').length;
      const errored = resp.results.filter((r) => r.status === 'error');
      if (aborted > 0) {
        showToast({
          title: `${aborted} ${aborted === 1 ? 'upload' : 'uploads'} aborted`,
        });
      }
      if (errored.length > 0) {
        showToast({
          title: `${errored.length} ${errored.length === 1 ? 'failure' : 'failures'}`,
          description: errored.map((e) => `${e.upload_id}: ${e.code ?? 'error'}`).join(', '),
          variant: 'destructive',
        });
      }
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: ['multipart', 'active'] });
    },
    onError: (err) => {
      showToast({
        title: 'Abort failed',
        description: err instanceof Error ? err.message : String(err),
        variant: 'destructive',
      });
    },
  });

  function handleAbortSelected() {
    const targets: { bucket: string; upload_id: string }[] = [];
    for (const r of rows) {
      if (selected.has(rowKey(r))) {
        targets.push({ bucket: r.bucket, upload_id: r.upload_id });
      }
    }
    if (targets.length === 0) return;
    if (!window.confirm(`Abort ${targets.length} selected multipart upload(s)?`)) return;
    abortM.mutate(targets);
  }

  function handleAbortOne(r: MultipartActiveRow) {
    if (!window.confirm(`Abort multipart upload ${r.upload_id}?`)) return;
    abortM.mutate([{ bucket: r.bucket, upload_id: r.upload_id }]);
  }

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: ['multipart', 'active'] });
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Multipart watchdog</h1>
        <p className="text-sm text-muted-foreground">
          In-flight multipart uploads cluster-wide. Polls every 30 seconds.
        </p>
      </div>

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load multipart uploads</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Filters</CardTitle>
          <CardDescription>
            Narrow the active-uploads list. Defaults to uploads at least 24 h old.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-3">
          <div>
            <Label className="text-xs" htmlFor="mp-filter-bucket">
              Bucket
            </Label>
            <Input
              id="mp-filter-bucket"
              value={bucket}
              onChange={(e) => setBucket(e.target.value)}
              placeholder="all buckets"
              list="mp-filter-bucket-suggestions"
            />
            <datalist id="mp-filter-bucket-suggestions">
              {bucketSuggestions.map((b) => (
                <option key={b} value={b} />
              ))}
            </datalist>
          </div>
          <div>
            <Label className="text-xs" htmlFor="mp-filter-min-age">
              Minimum age (hours)
            </Label>
            <Input
              id="mp-filter-min-age"
              type="number"
              min={0}
              value={minAgeHoursRaw}
              onChange={(e) => setMinAgeHoursRaw(e.target.value)}
              placeholder="24"
            />
          </div>
          <div>
            <Label className="text-xs" htmlFor="mp-filter-initiator">
              Initiator (access key)
            </Label>
            <Input
              id="mp-filter-initiator"
              value={initiator}
              onChange={(e) => setInitiator(e.target.value)}
              placeholder="any"
            />
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Active uploads</CardTitle>
            <CardDescription>
              {q.isFetching && !showSkeleton
                ? 'Refreshing…'
                : `${total} ${total === 1 ? 'upload' : 'uploads'} match · polling every 30 s`}
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="destructive"
              size="sm"
              disabled={!someVisibleSelected || abortM.isPending}
              onClick={handleAbortSelected}
            >
              <X className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              Abort selected{someVisibleSelected ? ` (${rows.filter((r) => selected.has(rowKey(r))).length})` : ''}
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching}
              aria-label="Refresh multipart uploads"
            >
              <RefreshCw
                className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
                aria-hidden
              />
              Refresh
            </Button>
          </div>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-10 pl-4 sm:pl-6">
                    <input
                      type="checkbox"
                      aria-label="Select all visible uploads"
                      checked={allVisibleSelected}
                      ref={(el) => {
                        if (el) el.indeterminate = !allVisibleSelected && someVisibleSelected;
                      }}
                      onChange={(e) => toggleAll(e.target.checked)}
                      className="h-4 w-4 cursor-pointer"
                    />
                  </TableHead>
                  <TableHead>Bucket</TableHead>
                  <TableHead>Key</TableHead>
                  <TableHead>Upload ID</TableHead>
                  <TableHead>Initiated</TableHead>
                  <TableHead className="text-right">Age</TableHead>
                  <TableHead>Storage class</TableHead>
                  <TableHead>Initiator</TableHead>
                  <TableHead className="text-right">Bytes uploaded</TableHead>
                  <TableHead className="pr-4 text-right sm:pr-6">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {showSkeleton &&
                  Array.from({ length: 4 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={10} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}
                {!showSkeleton && rows.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={10} className="py-10 text-center">
                      <div className="space-y-2">
                        <div className="text-sm font-medium">No active multipart uploads</div>
                        <div className="text-xs text-muted-foreground">
                          {minAgeHours > 0
                            ? `No uploads older than ${minAgeHours} h match the current filters.`
                            : 'No multipart uploads currently match the filters.'}
                        </div>
                      </div>
                    </TableCell>
                  </TableRow>
                )}
                {rows.map((r) => {
                  const isSelected = selected.has(rowKey(r));
                  return (
                    <TableRow key={rowKey(r)} data-state={isSelected ? 'selected' : undefined}>
                      <TableCell className="pl-4 sm:pl-6">
                        <input
                          type="checkbox"
                          aria-label={`Select upload ${r.upload_id}`}
                          checked={isSelected}
                          onChange={(e) => toggleOne(r, e.target.checked)}
                          className="h-4 w-4 cursor-pointer"
                        />
                      </TableCell>
                      <TableCell className="font-medium">{r.bucket}</TableCell>
                      <TableCell className="font-mono text-xs" title={r.key}>
                        {r.key}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground" title={r.upload_id}>
                        {r.upload_id.length > 18 ? `${r.upload_id.slice(0, 8)}…${r.upload_id.slice(-6)}` : r.upload_id}
                      </TableCell>
                      <TableCell title={new Date(r.initiated_at * 1000).toISOString()}>
                        {new Date(r.initiated_at * 1000).toLocaleString()}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{formatAge(r.age_seconds)}</TableCell>
                      <TableCell>{r.storage_class || '—'}</TableCell>
                      <TableCell className="font-mono text-xs">{r.initiator || '—'}</TableCell>
                      <TableCell className="text-right tabular-nums">{formatBytes(r.bytes_uploaded)}</TableCell>
                      <TableCell className="pr-4 text-right sm:pr-6">
                        <Button
                          type="button"
                          variant="ghost"
                          size="sm"
                          onClick={() => handleAbortOne(r)}
                          disabled={abortM.isPending}
                          className="text-destructive hover:text-destructive"
                          aria-label={`Abort upload ${r.upload_id}`}
                        >
                          <X className="h-3.5 w-3.5" aria-hidden />
                        </Button>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>
          {total > 0 && totalPages > 1 && (
            <div className="flex flex-col items-center justify-between gap-2 border-t px-4 py-3 text-sm text-muted-foreground sm:flex-row sm:px-6">
              <div>
                Page {page} of {totalPages} · {total} total
              </div>
              <div className="flex items-center gap-1.5">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setPage((p) => Math.max(1, p - 1))}
                  disabled={page <= 1 || q.isFetching}
                >
                  Previous
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                  disabled={page >= totalPages || q.isFetching}
                >
                  Next
                </Button>
              </div>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
