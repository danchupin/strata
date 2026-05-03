import { useEffect, useMemo, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { useQuery, keepPreviousData } from '@tanstack/react-query';
import {
  AlertCircle,
  ChevronRight,
  File,
  Folder,
  Home,
  RefreshCw,
  Search,
} from 'lucide-react';

import {
  fetchBucket,
  fetchObjects,
  type BucketDetail,
  type ObjectEntry,
  type ObjectsResponse,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
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

function useDebounced<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
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

function shortETag(etag: string): string {
  if (!etag) return '—';
  // Some backends emit the etag wrapped in quotes (S3 response shape).
  const trimmed = etag.replace(/^"+|"+$/g, '');
  return trimmed.slice(0, 8);
}

// Split the prefix into navigable breadcrumb segments. Trailing '/' included
// per segment so callers can rebuild the cumulative path with a join.
function breadcrumbSegments(prefix: string): Array<{ label: string; path: string }> {
  if (!prefix) return [];
  const parts = prefix.split('/').filter(Boolean);
  let acc = '';
  return parts.map((p) => {
    acc = `${acc}${p}/`;
    return { label: p, path: acc };
  });
}

export function BucketDetailPage() {
  const { name = '' } = useParams<{ name: string }>();
  const [searchParams, setSearchParams] = useSearchParams();
  const prefix = searchParams.get('prefix') ?? '';
  const [filter, setFilter] = useState('');
  const debouncedFilter = useDebounced(filter, FILTER_DEBOUNCE_MS);
  // markerStack is the navigation history of continuation tokens — index N is
  // the marker that started page N+1. Empty means we're on page 1.
  const [markerStack, setMarkerStack] = useState<string[]>([]);
  const [selected, setSelected] = useState<ObjectEntry | null>(null);

  // The active prefix is the URL prefix joined with the debounced filter so
  // the operator can drill in via folder click OR by typing a deeper prefix.
  const effectivePrefix = useMemo(() => {
    return prefix + debouncedFilter;
  }, [prefix, debouncedFilter]);

  // Reset pagination whenever the prefix or filter changes — older marker
  // tokens are meaningless for the new query.
  useEffect(() => {
    setMarkerStack([]);
  }, [prefix, debouncedFilter]);

  // Filter is reset when navigating up/down a folder so the input doesn't
  // carry a stale partial.
  useEffect(() => {
    setFilter('');
  }, [prefix]);

  const detailQ = useQuery({
    queryKey: queryKeys.buckets.one(name),
    queryFn: () => fetchBucket(name),
    enabled: Boolean(name),
    meta: { label: `bucket ${name}` },
  });

  const currentMarker = markerStack[markerStack.length - 1] ?? '';
  const objectsQ = useQuery<ObjectsResponse>({
    queryKey: queryKeys.buckets.objects(name, effectivePrefix, currentMarker),
    queryFn: () =>
      fetchObjects(name, {
        prefix: effectivePrefix || undefined,
        marker: currentMarker || undefined,
        pageSize: PAGE_SIZE,
      }),
    enabled: Boolean(name),
    placeholderData: keepPreviousData,
    meta: { label: `objects in ${name}` },
  });

  const detail = detailQ.data;
  const objects = objectsQ.data?.objects ?? [];
  const folders = objectsQ.data?.common_prefixes ?? [];
  const detailNotFound =
    detailQ.error instanceof Error && /404/.test(detailQ.error.message);

  function navigatePrefix(next: string) {
    setSelected(null);
    if (next) {
      setSearchParams({ prefix: next });
    } else {
      setSearchParams({});
    }
  }

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: queryKeys.buckets.one(name) });
    void queryClient.invalidateQueries({ queryKey: ['buckets', 'objects', name] });
  }

  function handleNextPage() {
    const nextMarker = objectsQ.data?.next_marker;
    if (nextMarker) {
      setMarkerStack((s) => [...s, nextMarker]);
    }
  }

  function handlePrevPage() {
    setMarkerStack((s) => s.slice(0, -1));
  }

  if (!name) return null;

  if (detailNotFound) {
    return (
      <div className="space-y-4">
        <Link
          to="/buckets"
          className="text-sm text-muted-foreground hover:text-foreground"
        >
          ← Back to buckets
        </Link>
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-6 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Bucket not found</div>
              <div className="text-xs text-destructive/80">
                <code>{name}</code> does not exist in this cluster.
              </div>
            </div>
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-2">
        <Link
          to="/buckets"
          className="text-xs text-muted-foreground hover:text-foreground"
        >
          ← Back to buckets
        </Link>
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="text-2xl font-semibold tracking-tight">{name}</h1>
          {detail && <BucketBadges detail={detail} />}
        </div>
      </div>

      <StatsBar detail={detail} loading={detailQ.isPending && !detail} />

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Objects</CardTitle>
            <CardDescription>
              {objectsQ.isFetching && objectsQ.data
                ? 'Refreshing…'
                : `${folders.length} ${folders.length === 1 ? 'folder' : 'folders'}, ${
                    objects.length
                  } ${objects.length === 1 ? 'object' : 'objects'} on this page`}
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <div className="relative">
              <Search
                className="pointer-events-none absolute left-2 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
                aria-hidden
              />
              <Input
                aria-label="Filter by prefix"
                placeholder="Filter prefix…"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                className="h-9 w-56 pl-8"
              />
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={objectsQ.isFetching}
              aria-label="Refresh objects"
            >
              <RefreshCw
                className={cn(
                  'mr-1.5 h-3.5 w-3.5',
                  objectsQ.isFetching && 'animate-spin',
                )}
                aria-hidden
              />
              Refresh
            </Button>
          </div>
        </CardHeader>

        <Breadcrumbs prefix={prefix} onNavigate={navigatePrefix} />

        <CardContent className="px-0 sm:px-0">
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4 sm:pl-6">Name</TableHead>
                  <TableHead className="text-right">Size</TableHead>
                  <TableHead>Last modified</TableHead>
                  <TableHead>Storage class</TableHead>
                  <TableHead className="pr-4 sm:pr-6">ETag</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {objectsQ.isPending && !objectsQ.data &&
                  Array.from({ length: 4 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={5} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}

                {!objectsQ.isPending &&
                  folders.length === 0 &&
                  objects.length === 0 && (
                    <TableRow>
                      <TableCell colSpan={5} className="py-10 text-center">
                        <div className="space-y-1">
                          <div className="text-sm font-medium">
                            This bucket is empty
                          </div>
                          <div className="text-xs text-muted-foreground">
                            {effectivePrefix
                              ? `No objects under "${effectivePrefix}".`
                              : 'No objects yet.'}
                          </div>
                        </div>
                      </TableCell>
                    </TableRow>
                  )}

                {folders.map((p) => (
                  <TableRow key={`folder-${p}`} className="cursor-pointer">
                    <TableCell className="pl-4 font-medium sm:pl-6">
                      <button
                        type="button"
                        onClick={() => navigatePrefix(p)}
                        className="inline-flex items-center gap-2 text-primary underline-offset-2 hover:underline"
                      >
                        <Folder className="h-4 w-4" aria-hidden />
                        {trimPrefix(p, prefix)}
                      </button>
                    </TableCell>
                    <TableCell className="text-right text-muted-foreground">—</TableCell>
                    <TableCell className="text-muted-foreground">—</TableCell>
                    <TableCell className="text-muted-foreground">—</TableCell>
                    <TableCell className="pr-4 text-muted-foreground sm:pr-6">—</TableCell>
                  </TableRow>
                ))}

                {objects.map((o) => (
                  <TableRow key={`obj-${o.key}`}>
                    <TableCell className="pl-4 font-medium sm:pl-6">
                      <button
                        type="button"
                        onClick={() => setSelected(o)}
                        className="inline-flex items-center gap-2 text-left text-primary underline-offset-2 hover:underline"
                      >
                        <File className="h-4 w-4" aria-hidden />
                        {trimPrefix(o.key, prefix)}
                      </button>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatBytes(o.size)}
                    </TableCell>
                    <TableCell
                      title={
                        o.last_modified
                          ? new Date(o.last_modified * 1000).toISOString()
                          : ''
                      }
                    >
                      {formatRelative(o.last_modified)}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {o.storage_class || '—'}
                    </TableCell>
                    <TableCell
                      className="pr-4 font-mono text-xs sm:pr-6"
                      title={o.etag}
                    >
                      {shortETag(o.etag)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>

          <div className="flex flex-col items-center justify-between gap-2 border-t px-4 py-3 text-sm text-muted-foreground sm:flex-row sm:px-6">
            <div>
              {objects.length + folders.length === 0
                ? 'No rows on this page'
                : `Page ${markerStack.length + 1}${
                    objectsQ.data?.is_truncated ? ' · more available' : ''
                  }`}
            </div>
            <div className="flex items-center gap-1.5">
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={handlePrevPage}
                disabled={markerStack.length === 0 || objectsQ.isFetching}
              >
                Previous
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={handleNextPage}
                disabled={
                  !objectsQ.data?.is_truncated ||
                  !objectsQ.data?.next_marker ||
                  objectsQ.isFetching
                }
              >
                Next
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>

      <ObjectDetailSheet
        bucket={name}
        object={selected}
        onClose={() => setSelected(null)}
      />
    </div>
  );
}

function trimPrefix(s: string, prefix: string): string {
  return prefix && s.startsWith(prefix) ? s.slice(prefix.length) : s;
}

function BucketBadges({ detail }: { detail: BucketDetail }) {
  const versioning = detail.versioning ?? 'Off';
  const versioningVariant: 'success' | 'warning' | 'secondary' =
    versioning === 'Enabled'
      ? 'success'
      : versioning === 'Suspended'
        ? 'warning'
        : 'secondary';
  return (
    <div className="flex items-center gap-2">
      <Badge variant={versioningVariant}>Versioning · {versioning}</Badge>
      <Badge variant={detail.object_lock ? 'success' : 'secondary'}>
        Object lock · {detail.object_lock ? 'On' : 'Off'}
      </Badge>
    </div>
  );
}

function StatsBar({
  detail,
  loading,
}: {
  detail: BucketDetail | undefined;
  loading: boolean;
}) {
  const items: Array<{ label: string; value: string }> = detail
    ? [
        { label: 'Size', value: formatBytes(detail.size_bytes) },
        { label: 'Objects', value: formatCount(detail.object_count) },
        { label: 'Region', value: detail.region || '—' },
        { label: 'Created', value: formatRelative(detail.created_at) },
      ]
    : [];
  return (
    <Card>
      <CardContent className="grid grid-cols-2 gap-4 py-4 sm:grid-cols-4">
        {loading &&
          Array.from({ length: 4 }).map((_, i) => (
            <div key={`stat-sk-${i}`} className="space-y-2">
              <Skeleton className="h-3 w-16" />
              <Skeleton className="h-5 w-24" />
            </div>
          ))}
        {!loading &&
          items.map((item) => (
            <div key={item.label}>
              <div className="text-xs uppercase tracking-wide text-muted-foreground">
                {item.label}
              </div>
              <div className="mt-1 text-base font-medium tabular-nums">
                {item.value}
              </div>
            </div>
          ))}
      </CardContent>
    </Card>
  );
}

function Breadcrumbs({
  prefix,
  onNavigate,
}: {
  prefix: string;
  onNavigate: (next: string) => void;
}) {
  const segments = breadcrumbSegments(prefix);
  return (
    <div className="flex items-center gap-1 px-4 pb-3 text-sm text-muted-foreground sm:px-6">
      <button
        type="button"
        onClick={() => onNavigate('')}
        className="inline-flex items-center gap-1 hover:text-foreground"
      >
        <Home className="h-3.5 w-3.5" aria-hidden />
        <span>root</span>
      </button>
      {segments.map((seg) => (
        <span key={seg.path} className="inline-flex items-center gap-1">
          <ChevronRight className="h-3.5 w-3.5" aria-hidden />
          <button
            type="button"
            onClick={() => onNavigate(seg.path)}
            className="hover:text-foreground"
          >
            {seg.label}
          </button>
        </span>
      ))}
    </div>
  );
}

function ObjectDetailSheet({
  bucket,
  object,
  onClose,
}: {
  bucket: string;
  object: ObjectEntry | null;
  onClose: () => void;
}) {
  return (
    <Sheet open={Boolean(object)} onOpenChange={(open) => !open && onClose()}>
      <SheetContent side="right" className="sm:max-w-md">
        {object && (
          <>
            <SheetHeader>
              <SheetTitle className="break-all">{object.key}</SheetTitle>
              <SheetDescription className="break-all">
                <code className="text-xs">{bucket}/{object.key}</code>
              </SheetDescription>
            </SheetHeader>
            <dl className="mt-4 grid grid-cols-1 gap-3 text-sm">
              <DetailRow label="Size" value={formatBytes(object.size)} />
              <DetailRow
                label="Last modified"
                value={
                  object.last_modified
                    ? new Date(object.last_modified * 1000).toLocaleString()
                    : '—'
                }
              />
              <DetailRow label="Storage class" value={object.storage_class || '—'} />
              <DetailRow
                label="ETag"
                value={
                  <code className="break-all font-mono text-xs">
                    {object.etag || '—'}
                  </code>
                }
              />
            </dl>
            <div className="mt-6 flex flex-col gap-2 sm:flex-row">
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled
                title="Coming in Phase 2"
              >
                Download
              </Button>
              <Button
                type="button"
                variant="destructive"
                size="sm"
                disabled
                title="Coming in Phase 2"
              >
                Delete
              </Button>
            </div>
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}

function DetailRow({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[120px_1fr] items-baseline gap-2">
      <dt className="text-xs uppercase tracking-wide text-muted-foreground">
        {label}
      </dt>
      <dd className="text-sm">{value}</dd>
    </div>
  );
}
