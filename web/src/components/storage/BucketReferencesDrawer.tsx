import { useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import {
  AlertCircle,
  ChevronRight,
  ExternalLink,
  Loader2,
  Wrench,
} from 'lucide-react';
import { Link } from 'react-router-dom';

import {
  fetchClusterDrainImpact,
  normalizePlacementMode,
  type BucketImpactEntry,
} from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
import { queryKeys } from '@/lib/query';
import { cn } from '@/lib/utils';

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
  // onOpenBulkFix opens the BulkPlacementFixDialog hosted by the parent
  // (ClustersSubsection). The drawer hands down the slice of by_bucket
  // rows whose category != 'migratable' so the dialog pre-selects them.
  // Apply invalidates clusterDrainImpact — the drawer's query refetches
  // automatically.
  onOpenBulkFix?: (stuck: BucketImpactEntry[]) => void;
}

// BucketReferencesDrawer renders the "Show affected buckets" side drawer
// opened from cluster cards. Consumes GET /admin/v1/clusters/{id}/drain-impact
// (US-001 drain-cleanup) so it surfaces every chunk on the cluster — including
// nil-policy buckets routed via class-env or default-weight — split into the
// three operator-meaningful categories: migratable (green), stuck single-policy
// (amber), stuck no-policy (amber).
export function BucketReferencesDrawer({
  open,
  onOpenChange,
  clusterID,
  onOpenBulkFix,
}: Props) {
  const [offset, setOffset] = useState(0);

  const q = useQuery({
    queryKey: queryKeys.clusterDrainImpactPage(clusterID, PAGE_SIZE, offset),
    queryFn: () => fetchClusterDrainImpact(clusterID, PAGE_SIZE, offset),
    enabled: open,
    placeholderData: keepPreviousData,
    meta: { label: `drain impact ${clusterID}` },
  });

  const errMsg = !q.data && q.error instanceof Error ? q.error.message : null;
  const data = q.data;
  const buckets = data?.by_bucket ?? [];
  const hasMore = data?.next_offset != null;

  const grouped = useMemo(() => {
    const migrating: BucketImpactEntry[] = [];
    const stuckSingle: BucketImpactEntry[] = [];
    const stuckNoPolicy: BucketImpactEntry[] = [];
    for (const b of buckets) {
      if (b.category === 'migratable') migrating.push(b);
      else if (b.category === 'stuck_single_policy') stuckSingle.push(b);
      else if (b.category === 'stuck_no_policy') stuckNoPolicy.push(b);
    }
    return { migrating, stuckSingle, stuckNoPolicy };
  }, [buckets]);

  // complianceLocked is the compliance-fix surface (US-005 effective-
  // placement): only strict-flagged stuck_single_policy buckets reach
  // BulkPlacementFixDialog because weighted buckets auto-resolve via
  // cluster.weights.
  const complianceLocked = useMemo<BucketImpactEntry[]>(
    () =>
      grouped.stuckSingle.filter(
        (b) => normalizePlacementMode(b.placement_mode) === 'strict',
      ),
    [grouped.stuckSingle],
  );
  const totalChunks = data?.total_chunks ?? 0;
  const totalBuckets = data?.total_buckets ?? buckets.length;

  return (
    <Sheet
      open={open}
      onOpenChange={(v) => {
        if (!v) setOffset(0);
        onOpenChange(v);
      }}
    >
      <SheetContent
        side="right"
        className="flex w-full flex-col gap-4 sm:max-w-lg"
        data-testid="bucket-references-drawer"
      >
        <SheetHeader>
          <SheetTitle>
            Drain impact — <code className="font-mono text-sm">{clusterID}</code>
          </SheetTitle>
          <SheetDescription>
            Every chunk on this cluster, categorized by whether the rebalance
            worker can migrate it under the current Placement policies. Stuck
            chunks block the evacuate path until their bucket policies are
            updated.
          </SheetDescription>
        </SheetHeader>

        {errMsg && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load drain impact</div>
              <div className="text-xs text-destructive/80">{errMsg}</div>
            </div>
          </div>
        )}

        {q.isPending && !data ? (
          <div
            className="flex items-center gap-2 text-sm text-muted-foreground"
            data-testid="bucket-references-loading"
          >
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> Loading…
          </div>
        ) : totalChunks === 0 ? (
          <div
            className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground"
            data-testid="bucket-references-empty"
          >
            No chunks on this cluster — safe to drain or remove.
          </div>
        ) : (
          <div className="flex-1 space-y-3 overflow-y-auto">
            <div className="text-xs text-muted-foreground">
              Showing{' '}
              <span className="font-medium text-foreground">
                {offset + 1}–{offset + buckets.length}
              </span>{' '}
              of <span className="font-medium text-foreground">{totalBuckets}</span>
            </div>

            {onOpenBulkFix && complianceLocked.length > 0 && (
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="w-full border-amber-500/60 text-amber-800 hover:bg-amber-500/15 dark:text-amber-200"
                onClick={() => onOpenBulkFix(complianceLocked)}
                data-testid="bucket-references-bulk-fix"
              >
                <Wrench className="mr-1 h-3.5 w-3.5" aria-hidden />
                Fix {complianceLocked.length} compliance-locked{' '}
                {complianceLocked.length === 1 ? 'bucket' : 'buckets'}
              </Button>
            )}

            <CategorySection
              testid="cat-migrating"
              tone="ok"
              title="Migrating"
              subtitle={`${(data?.migratable_chunks ?? 0).toLocaleString()} chunks`}
              rows={grouped.migrating}
              emptyText="No migratable chunks in this page."
              onClose={() => onOpenChange(false)}
            />
            <CategorySection
              testid="cat-stuck-single"
              tone="amber"
              title="Stuck — single-policy"
              subtitle={`${(data?.stuck_single_policy_chunks ?? 0).toLocaleString()} chunks`}
              rows={grouped.stuckSingle}
              emptyText="No single-policy stuck chunks in this page."
              onClose={() => onOpenChange(false)}
            />
            <CategorySection
              testid="cat-stuck-no-policy"
              tone="amber"
              title="Stuck — no policy"
              subtitle={`${(data?.stuck_no_policy_chunks ?? 0).toLocaleString()} chunks`}
              rows={grouped.stuckNoPolicy}
              emptyText="No no-policy stuck chunks in this page."
              onClose={() => onOpenChange(false)}
            />
          </div>
        )}

        <div className="flex items-center justify-between gap-2 border-t pt-3 text-xs">
          <span className="text-muted-foreground">Page size {PAGE_SIZE}</span>
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

interface CategorySectionProps {
  testid: string;
  tone: 'ok' | 'amber';
  title: string;
  subtitle: string;
  rows: BucketImpactEntry[];
  emptyText: string;
  onClose: () => void;
}

function CategorySection({
  testid,
  tone,
  title,
  subtitle,
  rows,
  emptyText,
  onClose,
}: CategorySectionProps) {
  const [open, setOpen] = useState(true);
  const headerClass =
    tone === 'ok'
      ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-800 dark:text-emerald-300'
      : 'border-amber-500/40 bg-amber-500/10 text-amber-800 dark:text-amber-300';
  return (
    <section data-testid={testid} className="rounded-md border">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className={cn(
          'flex w-full items-center justify-between gap-2 rounded-t-md px-3 py-2 text-left text-xs',
          headerClass,
        )}
      >
        <span className="font-medium">{title}</span>
        <span className="flex items-center gap-2 tabular-nums">
          <span>{subtitle}</span>
          <span>· {rows.length}</span>
          <ChevronRight
            className={cn(
              'h-3.5 w-3.5 transition-transform',
              open && 'rotate-90',
            )}
            aria-hidden
          />
        </span>
      </button>
      {open && (
        <div className="border-t">
          {rows.length === 0 ? (
            <div className="p-3 text-xs text-muted-foreground">{emptyText}</div>
          ) : (
            <ul className="divide-y">
              {rows.map((b) => (
                <BucketRow key={b.name} bucket={b} onClose={onClose} />
              ))}
            </ul>
          )}
        </div>
      )}
    </section>
  );
}

interface BucketRowProps {
  bucket: BucketImpactEntry;
  onClose: () => void;
}

function BucketRow({ bucket, onClose }: BucketRowProps) {
  const suggestions = (bucket.suggested_policies ?? []).slice(0, 2);
  return (
    <li
      className="flex items-start justify-between gap-3 p-3 text-sm"
      data-testid="bucket-references-row"
    >
      <div className="min-w-0 flex-1">
        <Link
          to={`/buckets/${encodeURIComponent(bucket.name)}?tab=placement`}
          className="block truncate font-mono font-medium text-foreground hover:underline"
          onClick={onClose}
          title="View Placement"
        >
          {bucket.name}
        </Link>
        <div className="mt-1 flex flex-wrap items-center gap-3 text-xs text-muted-foreground tabular-nums">
          <span>{bucket.chunk_count.toLocaleString()} chunks</span>
          <span>{formatBytes(bucket.bytes_used)}</span>
        </div>
        {suggestions.length > 0 && (
          <div className="mt-1.5 flex flex-wrap items-center gap-1">
            {suggestions.map((s) => (
              <Badge
                key={s.label}
                variant="outline"
                className="text-[10px] font-normal"
                title="Suggested remediation policy"
              >
                {s.label}
              </Badge>
            ))}
          </div>
        )}
      </div>
      <Link
        to={`/buckets/${encodeURIComponent(bucket.name)}?tab=placement`}
        onClick={onClose}
        className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
      >
        Placement <ExternalLink className="h-3 w-3" aria-hidden />
      </Link>
    </li>
  );
}
