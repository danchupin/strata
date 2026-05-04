import { useEffect, useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, Download, RefreshCw } from 'lucide-react';

import {
  auditCSVUrl,
  fetchAuditLog,
  fetchBucketsList,
  fetchIAMUsers,
  type AuditQuery,
  type AuditRecord,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
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

const PAGE_SIZE = 100;
const FILTER_DEBOUNCE_MS = 300;
const POLL_INTERVAL_MS = 60_000;

// Action choices preserved in primaryNav order — operators recognise the
// AWS-style verbs faster than alphabetised. Matches the Action stamp shape
// emitted by internal/s3api/audit.go::deriveAuditAction + admin handlers'
// `admin:<Verb>` overrides.
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
  'admin:ForceEmpty',
  'admin:SetBucketLifecycle',
  'admin:SetBucketCORS',
  'admin:DeleteBucketCORS',
  'admin:SetBucketPolicy',
  'admin:DeleteBucketPolicy',
  'admin:SetBucketACL',
  'admin:SetBucketLogging',
  'admin:DeleteBucketLogging',
  'admin:SetBucketObjectLock',
  'admin:SetBucketVersioning',
  'admin:SetInventory',
  'admin:DeleteInventory',
  'admin:CreateIAMUser',
  'admin:DeleteIAMUser',
  'admin:CreateAccessKey',
  'admin:DeleteAccessKey',
  'admin:UpdateAccessKey',
  'admin:CreateManagedPolicy',
  'admin:UpdateManagedPolicy',
  'admin:DeleteManagedPolicy',
  'admin:AttachUserPolicy',
  'admin:DetachUserPolicy',
  'admin:AbortMultipartUpload',
  'admin:ListAudit',
  'admin:ExportAuditCSV',
];

function useDebounced<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}

// defaultSinceISO returns "now - 24h" as an ISO string trimmed for the
// datetime-local input format (YYYY-MM-DDTHH:mm). The visible default ranges
// the table over the last 24 h, matching the PRD AC.
function defaultSinceLocal(): string {
  return formatLocalInput(new Date(Date.now() - 24 * 60 * 60 * 1000));
}

function formatLocalInput(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// localInputToRFC3339 converts a `<input type="datetime-local">` value to an
// RFC3339 string the admin endpoint accepts. Empty input → empty string.
function localInputToRFC3339(local: string): string {
  if (!local) return '';
  const d = new Date(local);
  if (Number.isNaN(d.getTime())) return '';
  return d.toISOString();
}

function rowKey(r: AuditRecord): string {
  return r.event_id;
}

function shortenID(id: string, head = 8, tail = 4): string {
  if (id.length <= head + tail + 1) return id;
  return `${id.slice(0, head)}…${id.slice(-tail)}`;
}

export function AuditLogPage() {
  const [sinceLocal, setSinceLocal] = useState<string>(defaultSinceLocal);
  const [untilLocal, setUntilLocal] = useState<string>('');
  const [actionsSelected, setActionsSelected] = useState<Set<string>>(new Set());
  const [actionsPickerOpen, setActionsPickerOpen] = useState(false);
  const [principal, setPrincipal] = useState('');
  const [bucket, setBucket] = useState('');
  const [pageTokens, setPageTokens] = useState<string[]>(['']);

  const debouncedPrincipal = useDebounced(principal.trim(), FILTER_DEBOUNCE_MS);
  const debouncedBucket = useDebounced(bucket.trim(), FILTER_DEBOUNCE_MS);
  const sinceRFC = useMemo(() => localInputToRFC3339(sinceLocal), [sinceLocal]);
  const untilRFC = useMemo(() => localInputToRFC3339(untilLocal), [untilLocal]);
  const actionParam = useMemo(
    () => Array.from(actionsSelected).sort().join(','),
    [actionsSelected],
  );

  // Reset pagination when any filter changes.
  useEffect(() => {
    setPageTokens(['']);
  }, [sinceRFC, untilRFC, actionParam, debouncedPrincipal, debouncedBucket]);

  const currentToken = pageTokens[pageTokens.length - 1] ?? '';

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

  const queryArgs: AuditQuery = useMemo(
    () => ({
      since: sinceRFC || undefined,
      until: untilRFC || undefined,
      action: actionParam || undefined,
      principal: debouncedPrincipal || undefined,
      bucket: debouncedBucket || undefined,
      pageToken: currentToken || undefined,
      limit: PAGE_SIZE,
    }),
    [sinceRFC, untilRFC, actionParam, debouncedPrincipal, debouncedBucket, currentToken],
  );

  const q = useQuery({
    queryKey: queryKeys.audit.list(
      sinceRFC,
      untilRFC,
      actionParam,
      debouncedPrincipal,
      debouncedBucket,
      currentToken,
    ),
    queryFn: () => fetchAuditLog(queryArgs),
    refetchInterval: POLL_INTERVAL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'audit log' },
  });

  const rows = q.data?.records ?? [];
  const nextToken = q.data?.next_page_token ?? '';
  const showSkeleton = q.isPending && !q.data;
  const errorMessage = !q.data && q.error instanceof Error ? q.error.message : null;

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: ['audit'] });
  }

  function handleLoadMore() {
    if (!nextToken) return;
    setPageTokens((prev) => [...prev, nextToken]);
  }

  function handleResetPaging() {
    setPageTokens(['']);
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

  function handleExportCSV() {
    // Drop pageToken — CSV is a server-paginated full export of the matching window.
    const csvArgs: AuditQuery = { ...queryArgs };
    delete csvArgs.pageToken;
    delete csvArgs.limit;
    window.location.href = auditCSVUrl(csvArgs);
  }

  const pageNumber = pageTokens.length;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Audit log</h1>
        <p className="text-sm text-muted-foreground">
          Append-only record of every state-changing request. Read paths
          (GET/HEAD) are not audited.
        </p>
      </div>

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load audit log</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Filters</CardTitle>
          <CardDescription>
            Narrow the audit feed. Defaults to the last 24 hours.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          <div>
            <Label className="text-xs" htmlFor="audit-filter-since">
              Since
            </Label>
            <Input
              id="audit-filter-since"
              type="datetime-local"
              value={sinceLocal}
              onChange={(e) => setSinceLocal(e.target.value)}
            />
          </div>
          <div>
            <Label className="text-xs" htmlFor="audit-filter-until">
              Until
            </Label>
            <Input
              id="audit-filter-until"
              type="datetime-local"
              value={untilLocal}
              onChange={(e) => setUntilLocal(e.target.value)}
              placeholder="now"
            />
          </div>
          <div>
            <Label className="text-xs" htmlFor="audit-filter-principal">
              Principal
            </Label>
            <Input
              id="audit-filter-principal"
              value={principal}
              onChange={(e) => setPrincipal(e.target.value)}
              placeholder="any"
              list="audit-filter-principal-suggestions"
            />
            <datalist id="audit-filter-principal-suggestions">
              {principalSuggestions.map((p) => (
                <option key={p} value={p} />
              ))}
            </datalist>
          </div>
          <div>
            <Label className="text-xs" htmlFor="audit-filter-bucket">
              Bucket
            </Label>
            <Input
              id="audit-filter-bucket"
              value={bucket}
              onChange={(e) => setBucket(e.target.value)}
              placeholder="any"
              list="audit-filter-bucket-suggestions"
            />
            <datalist id="audit-filter-bucket-suggestions">
              {bucketSuggestions.map((b) => (
                <option key={b} value={b} />
              ))}
            </datalist>
          </div>
          <div className="sm:col-span-2 lg:col-span-1">
            <Label className="text-xs">Actions</Label>
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
            <CardTitle className="text-base">Events</CardTitle>
            <CardDescription>
              {q.isFetching && !showSkeleton
                ? 'Refreshing…'
                : `Page ${pageNumber} · ${rows.length} ${rows.length === 1 ? 'row' : 'rows'} · auto-refresh 60 s`}
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleExportCSV}
              disabled={showSkeleton}
            >
              <Download className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              Export CSV
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching}
              aria-label="Refresh audit log"
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
                  <TableHead>Time</TableHead>
                  <TableHead>Request ID</TableHead>
                  <TableHead>Principal</TableHead>
                  <TableHead>Action</TableHead>
                  <TableHead>Resource</TableHead>
                  <TableHead className="text-right">Result</TableHead>
                  <TableHead>Source IP</TableHead>
                  <TableHead>User-Agent</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {showSkeleton &&
                  Array.from({ length: 6 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={8} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}
                {!showSkeleton && rows.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={8} className="py-10 text-center">
                      <div className="space-y-2">
                        <div className="text-sm font-medium">No audit events</div>
                        <div className="text-xs text-muted-foreground">
                          No rows match the current filters in the selected window.
                        </div>
                      </div>
                    </TableCell>
                  </TableRow>
                )}
                {rows.map((r) => (
                  <TableRow key={rowKey(r)}>
                    <TableCell title={r.time}>
                      {new Date(r.time).toLocaleString()}
                    </TableCell>
                    <TableCell
                      className="font-mono text-xs"
                      title="Trace available in Phase 3"
                    >
                      {shortenID(r.request_id || '—', 6, 4)}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {r.principal || '—'}
                    </TableCell>
                    <TableCell className="font-mono text-xs">{r.action}</TableCell>
                    <TableCell className="font-mono text-xs" title={r.resource}>
                      {r.resource}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {r.result || '—'}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {r.source_ip || '—'}
                    </TableCell>
                    <TableCell
                      className="max-w-[18rem] truncate font-mono text-xs"
                      title={r.user_agent}
                    >
                      {r.user_agent || '—'}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <div className="flex flex-col items-center justify-between gap-2 border-t px-4 py-3 text-sm text-muted-foreground sm:flex-row sm:px-6">
            <div>
              Page {pageNumber}
              {pageNumber > 1 && (
                <button
                  type="button"
                  onClick={handleResetPaging}
                  className="ml-2 underline-offset-2 hover:underline"
                >
                  back to first
                </button>
              )}
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleLoadMore}
              disabled={!nextToken || q.isFetching}
            >
              Load more
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
