import { ExternalLink } from 'lucide-react';
import { Link } from 'react-router-dom';

import type { BucketDrainProgressEntry } from '@/api/client';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';

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
  title: string;
  description: string;
  // rows are the pre-filtered stuck-bucket entries straight off the
  // /drain-progress by_bucket payload. The drawer does not refetch —
  // the DrainProgressBar's TanStack query is the source of truth and
  // re-renders this drawer on the 30s poll.
  rows: BucketDrainProgressEntry[];
}

// StuckBucketsDrawer mirrors the BucketReferencesDrawer shape from the
// placement-ui cycle (US-006 drain-lifecycle reused for US-006
// drain-transparency). Each row links to the BucketDetail Placement tab
// in a new tab so the operator can fix policy without losing the drain
// modal context.
export function StuckBucketsDrawer({
  open,
  onOpenChange,
  clusterID,
  title,
  description,
  rows,
}: Props) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex w-full flex-col gap-4 sm:max-w-lg"
        data-testid="stuck-buckets-drawer"
      >
        <SheetHeader>
          <SheetTitle>{title}</SheetTitle>
          <SheetDescription>
            {description} Cluster:{' '}
            <code className="font-mono text-sm">{clusterID}</code>.
          </SheetDescription>
        </SheetHeader>

        {rows.length === 0 ? (
          <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
            No buckets in this category right now. The next rebalance
            worker tick may add or remove rows.
          </div>
        ) : (
          <div className="flex-1 overflow-y-auto">
            <div className="text-xs text-muted-foreground">
              <span className="font-medium text-foreground">{rows.length}</span>{' '}
              stuck {rows.length === 1 ? 'bucket' : 'buckets'}
            </div>
            <ul className="mt-2 divide-y rounded-md border">
              {rows.map((b) => (
                <li
                  key={b.name}
                  className="flex items-center justify-between gap-3 p-3 text-sm"
                  data-testid="stuck-bucket-row"
                >
                  <div className="min-w-0 flex-1">
                    <div className="block truncate font-mono font-medium text-foreground">
                      {b.name}
                    </div>
                    <div className="mt-1 flex flex-wrap items-center gap-3 text-xs text-muted-foreground tabular-nums">
                      <span>
                        {b.chunk_count.toLocaleString()} chunks
                      </span>
                      <span>{formatBytes(b.bytes_used)}</span>
                    </div>
                  </div>
                  <Link
                    to={`/buckets/${encodeURIComponent(b.name)}?tab=placement`}
                    target="_blank"
                    rel="noopener"
                    className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
                    data-testid="stuck-bucket-edit"
                  >
                    Edit policy <ExternalLink className="h-3 w-3" aria-hidden />
                  </Link>
                </li>
              ))}
            </ul>
          </div>
        )}
      </SheetContent>
    </Sheet>
  );
}
