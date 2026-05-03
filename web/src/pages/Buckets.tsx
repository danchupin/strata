import { useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { useQuery, keepPreviousData } from '@tanstack/react-query';
import {
  AlertCircle,
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  RefreshCw,
  Search,
} from 'lucide-react';

import { fetchBucketsList, type BucketSummary } from '@/api/client';
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

type SortColumn = 'name' | 'owner' | 'created' | 'size' | 'object_count';
type SortOrder = 'asc' | 'desc';

const PAGE_SIZE = 50;
const SEARCH_DEBOUNCE_MS = 300;

const COLUMNS: Array<{
  key: SortColumn;
  label: string;
  className?: string;
  sortable?: boolean;
}> = [
  { key: 'name', label: 'Name', sortable: true },
  { key: 'owner', label: 'Owner', sortable: true },
  { key: 'created', label: 'Region', sortable: false },
  { key: 'created', label: 'Created', sortable: true },
  { key: 'size', label: 'Size', sortable: true, className: 'text-right' },
  {
    key: 'object_count',
    label: 'Object count',
    sortable: true,
    className: 'text-right',
  },
];

function defaultOrder(col: SortColumn): SortOrder {
  return col === 'name' || col === 'owner' ? 'asc' : 'desc';
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

function formatCount(n: number): string {
  if (!Number.isFinite(n)) return '0';
  return n.toLocaleString();
}

function formatRelative(epochSec: number): string {
  if (!epochSec) return '—';
  const ms = epochSec * 1000;
  const diff = Date.now() - ms;
  const d = new Date(ms);
  const iso = d.toLocaleString();
  if (diff < 0 || diff > 30 * 86400 * 1000) return iso;
  const sec = Math.floor(diff / 1000);
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  return `${day}d ago`;
}

function useDebounced<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}

function SortIcon({
  active,
  order,
}: {
  active: boolean;
  order: SortOrder;
}) {
  if (!active) {
    return (
      <ArrowUpDown className="ml-1 inline h-3.5 w-3.5 text-muted-foreground/60" aria-hidden />
    );
  }
  return order === 'asc' ? (
    <ArrowUp className="ml-1 inline h-3.5 w-3.5" aria-hidden />
  ) : (
    <ArrowDown className="ml-1 inline h-3.5 w-3.5" aria-hidden />
  );
}

export function BucketsPage() {
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebounced(search, SEARCH_DEBOUNCE_MS);
  const [sort, setSort] = useState<SortColumn>('created');
  const [order, setOrder] = useState<SortOrder>('desc');
  const [page, setPage] = useState(1);

  // Reset to page 1 whenever the filter or sort changes — the row count under
  // page 2 is meaningless for the new query.
  useEffect(() => {
    setPage(1);
  }, [debouncedSearch, sort, order]);

  const params = useMemo(
    () => ({
      query: debouncedSearch || undefined,
      sort,
      order,
      page,
      pageSize: PAGE_SIZE,
    }),
    [debouncedSearch, sort, order, page],
  );

  const q = useQuery({
    queryKey: queryKeys.buckets.list(
      debouncedSearch,
      sort,
      order,
      page,
      PAGE_SIZE,
    ),
    queryFn: () => fetchBucketsList(params),
    placeholderData: keepPreviousData,
    meta: { label: 'buckets list' },
  });

  const buckets: BucketSummary[] = q.data?.buckets ?? [];
  const total = q.data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const showSkeleton = q.isPending && !q.data;
  const errorMessage =
    !q.data && q.error instanceof Error ? q.error.message : null;

  function handleSort(col: SortColumn) {
    if (col === sort) {
      setOrder((o) => (o === 'asc' ? 'desc' : 'asc'));
    } else {
      setSort(col);
      setOrder(defaultOrder(col));
    }
  }

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: ['buckets', 'list'] });
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Buckets</h1>
        <p className="text-sm text-muted-foreground">
          List, search, and inspect every bucket in the cluster.
        </p>
      </div>

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load buckets</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">All buckets</CardTitle>
            <CardDescription>
              {q.isFetching && !showSkeleton
                ? 'Refreshing…'
                : `${total} ${total === 1 ? 'bucket' : 'buckets'} total`}
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <div className="relative">
              <Search
                className="pointer-events-none absolute left-2 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
                aria-hidden
              />
              <Input
                aria-label="Search buckets"
                placeholder="Search buckets…"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                className="h-9 w-56 pl-8"
              />
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching}
              aria-label="Refresh buckets"
            >
              <RefreshCw
                className={cn(
                  'mr-1.5 h-3.5 w-3.5',
                  q.isFetching && 'animate-spin',
                )}
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
                  {COLUMNS.map((c, idx) => {
                    const active = Boolean(c.sortable && c.key === sort);
                    return (
                      <TableHead
                        key={`${c.label}-${idx}`}
                        className={cn(
                          idx === 0 && 'pl-4 sm:pl-6',
                          idx === COLUMNS.length - 1 && 'pr-4 sm:pr-6',
                          c.className,
                        )}
                      >
                        {c.sortable ? (
                          <button
                            type="button"
                            onClick={() => handleSort(c.key)}
                            className={cn(
                              'inline-flex items-center font-medium hover:text-foreground',
                              active ? 'text-foreground' : 'text-muted-foreground',
                            )}
                            aria-sort={
                              active
                                ? order === 'asc'
                                  ? 'ascending'
                                  : 'descending'
                                : 'none'
                            }
                          >
                            {c.label}
                            <SortIcon active={active} order={order} />
                          </button>
                        ) : (
                          <span className="font-medium text-muted-foreground">
                            {c.label}
                          </span>
                        )}
                      </TableHead>
                    );
                  })}
                </TableRow>
              </TableHeader>
              <TableBody>
                {showSkeleton &&
                  Array.from({ length: 4 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={COLUMNS.length} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}
                {!showSkeleton && buckets.length === 0 && (
                  <TableRow>
                    <TableCell
                      colSpan={COLUMNS.length}
                      className="py-10 text-center"
                    >
                      <div className="space-y-2">
                        <div className="text-sm font-medium">No buckets</div>
                        <div className="text-xs text-muted-foreground">
                          {debouncedSearch
                            ? `No buckets match "${debouncedSearch}".`
                            : 'No buckets exist in this cluster yet.'}
                        </div>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          disabled
                          title="Coming in Phase 2"
                          className="mt-2"
                        >
                          Create your first bucket
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                )}
                {buckets.map((b) => (
                  <TableRow key={b.name}>
                    <TableCell className="pl-4 font-medium sm:pl-6">
                      <Link
                        to={`/buckets/${encodeURIComponent(b.name)}`}
                        className="text-primary underline-offset-2 hover:underline"
                      >
                        {b.name}
                      </Link>
                    </TableCell>
                    <TableCell>{b.owner || '—'}</TableCell>
                    <TableCell className="text-muted-foreground">
                      {b.region || '—'}
                    </TableCell>
                    <TableCell title={
                      b.created_at
                        ? new Date(b.created_at * 1000).toISOString()
                        : ''
                    }>
                      {formatRelative(b.created_at)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatBytes(b.size_bytes)}
                    </TableCell>
                    <TableCell className="pr-4 text-right tabular-nums sm:pr-6">
                      {formatCount(b.object_count)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          {total > 0 && (
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
                <PageNumbers
                  page={page}
                  totalPages={totalPages}
                  onPick={setPage}
                />
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

// PageNumbers renders a compact page picker. Up to 5 surrounding pages plus
// first/last sentinels with ellipses when the range is wider.
function PageNumbers({
  page,
  totalPages,
  onPick,
}: {
  page: number;
  totalPages: number;
  onPick: (n: number) => void;
}) {
  const window = 2;
  const start = Math.max(1, page - window);
  const end = Math.min(totalPages, page + window);
  const pages: Array<number | 'gap'> = [];
  if (start > 1) {
    pages.push(1);
    if (start > 2) pages.push('gap');
  }
  for (let p = start; p <= end; p++) pages.push(p);
  if (end < totalPages) {
    if (end < totalPages - 1) pages.push('gap');
    pages.push(totalPages);
  }
  return (
    <div className="hidden items-center gap-1 sm:flex">
      {pages.map((p, i) =>
        p === 'gap' ? (
          <span key={`gap-${i}`} className="px-1 text-muted-foreground">
            …
          </span>
        ) : (
          <Button
            key={p}
            type="button"
            variant={p === page ? 'default' : 'outline'}
            size="sm"
            onClick={() => onPick(p)}
            className="h-8 min-w-8 px-2"
          >
            {p}
          </Button>
        ),
      )}
    </div>
  );
}
