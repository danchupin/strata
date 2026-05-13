import { useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, ChevronRight, ExternalLink, Loader2 } from 'lucide-react';
import { Link } from 'react-router-dom';

import { fetchClusterBucketReferences } from '@/api/client';
import { Button } from '@/components/ui/button';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
import { queryKeys } from '@/lib/query';

const PAGE_SIZE = 100;

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

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  clusterID: string;
}

// BucketReferencesDrawer renders the "Show affected buckets" side drawer
// opened from cluster cards (US-006 drain-lifecycle). Lists every bucket
// whose Placement policy carries a non-zero weight on the cluster, with
// chunk_count + bytes_used from the live bucket_stats counter. Sorted
// desc by chunk_count, asc by name on ties.
export function BucketReferencesDrawer({ open, onOpenChange, clusterID }: Props) {
  const [offset, setOffset] = useState(0);

  const q = useQuery({
    queryKey: queryKeys.clusterBucketRefs(clusterID, PAGE_SIZE, offset),
    queryFn: () => fetchClusterBucketReferences(clusterID, PAGE_SIZE, offset),
    enabled: open,
    placeholderData: keepPreviousData,
    meta: { label: `bucket references ${clusterID}` },
  });

  const errMsg = !q.data && q.error instanceof Error ? q.error.message : null;
  const total = q.data?.total_buckets ?? 0;
  const buckets = q.data?.buckets ?? [];
  const hasMore = q.data?.next_offset != null;

  return (
    <Sheet
      open={open}
      onOpenChange={(v) => {
        if (!v) setOffset(0);
        onOpenChange(v);
      }}
    >
      <SheetContent side="right" className="flex w-full flex-col gap-4 sm:max-w-lg">
        <SheetHeader>
          <SheetTitle>
            Buckets referencing <code className="font-mono text-sm">{clusterID}</code>
          </SheetTitle>
          <SheetDescription>
            Each bucket below routes new PUTs through this cluster via its
            Placement policy. Drain the cluster and the rebalance worker
            migrates these buckets' existing chunks; new PUTs from a
            zero-alternate policy fall back to the class default (or refuse
            with 503 in strict mode).
          </SheetDescription>
        </SheetHeader>

        {errMsg && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load bucket references</div>
              <div className="text-xs text-destructive/80">{errMsg}</div>
            </div>
          </div>
        )}

        {q.isPending && !q.data ? (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> Loading…
          </div>
        ) : total === 0 ? (
          <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
            No buckets reference this cluster in their Placement policy.
            Default routing buckets are not listed — drain only stops new
            chunk writes for buckets that explicitly target this cluster.
          </div>
        ) : (
          <div className="flex-1 overflow-y-auto">
            <div className="text-xs text-muted-foreground">
              Showing{' '}
              <span className="font-medium text-foreground">
                {offset + 1}–{offset + buckets.length}
              </span>{' '}
              of <span className="font-medium text-foreground">{total}</span>
            </div>
            <ul className="mt-2 divide-y rounded-md border">
              {buckets.map((b) => (
                <li
                  key={b.name}
                  className="flex items-center justify-between gap-3 p-3 text-sm"
                >
                  <div className="min-w-0 flex-1">
                    <Link
                      to={`/buckets/${encodeURIComponent(b.name)}?tab=placement`}
                      className="block truncate font-mono font-medium text-foreground hover:underline"
                      onClick={() => onOpenChange(false)}
                      title="View Placement"
                    >
                      {b.name}
                    </Link>
                    <div className="mt-1 flex flex-wrap items-center gap-3 text-xs text-muted-foreground tabular-nums">
                      <span>
                        weight{' '}
                        <span className="font-medium text-foreground">
                          {b.weight}
                        </span>
                      </span>
                      <span>
                        {b.chunk_count.toLocaleString()} objects
                      </span>
                      <span>{formatBytes(b.bytes_used)}</span>
                    </div>
                  </div>
                  <Link
                    to={`/buckets/${encodeURIComponent(b.name)}/placement`}
                    onClick={() => onOpenChange(false)}
                    className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
                  >
                    Placement <ExternalLink className="h-3 w-3" aria-hidden />
                  </Link>
                </li>
              ))}
            </ul>
          </div>
        )}

        <div className="flex items-center justify-between gap-2 border-t pt-3 text-xs">
          <span className="text-muted-foreground">
            Page size {PAGE_SIZE}
          </span>
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setOffset((v) => Math.max(0, v - PAGE_SIZE))}
              disabled={offset === 0 || q.isFetching}
            >
              Prev
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setOffset((v) => v + PAGE_SIZE)}
              disabled={!hasMore || q.isFetching}
            >
              Next <ChevronRight className="ml-1 h-3 w-3" aria-hidden />
            </Button>
          </div>
        </div>
      </SheetContent>
    </Sheet>
  );
}
