import { useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertTriangle, ExternalLink, Info, RefreshCw } from 'lucide-react';

import {
  fetchAuditLog,
  fetchHotShards,
  type AdminApiError,
  type AuditRecord,
  type BucketDetail,
  type HotShardSeries,
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
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';
import { Heatmap, type HeatmapClick, type HeatmapRow } from '@/components/Heatmap';

const POLL_INTERVAL_MS = 30_000;
// Audit `resource` strings are truncated to 200 chars at write-time
// (Cassandra schema limit). Any key shown in the drill panel that hits the
// limit gets a truncation tooltip — matches the AC.
const AUDIT_RESOURCE_TRUNCATE_LEN = 200;

type RangeKey = '15m' | '1h' | '6h' | '24h';
interface RangeOption {
  key: RangeKey;
  label: string;
  range: string;
  step: string;
}

const RANGE_OPTIONS: RangeOption[] = [
  { key: '15m', label: '15m', range: '15m', step: '1m' },
  { key: '1h', label: '1h', range: '1h', step: '5m' },
  { key: '6h', label: '6h', range: '6h', step: '30m' },
  { key: '24h', label: '24h', range: '24h', step: '1h' },
];

function isMetricsUnavailable(err: unknown): boolean {
  return (
    err != null &&
    typeof err === 'object' &&
    (err as AdminApiError).code === 'MetricsUnavailable'
  );
}

function toHeatmapRows(matrix: HotShardSeries[]): HeatmapRow[] {
  return matrix.map((s) => ({
    label: `shard ${s.shard}`,
    values: s.values.map((p) => ({ ts: p.ts, value: p.value })),
  }));
}

// fnv1a32 mirrors Go's hash/fnv New32a — the function `internal/meta/cassandra/
// store.go::shardOf` runs at write-time. Reproducing it client-side keeps the
// drill panel shard reconstruction in lockstep without a round-trip.
function fnv1a32(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i) & 0xff;
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h >>> 0;
}

function shardOf(key: string, n: number): number {
  if (n <= 0) return 0;
  return fnv1a32(key) % n;
}

// resourceToKey strips the `/<bucket>/` prefix the audit middleware stamps
// onto S3 object resources so the remaining string is the object key the
// shard is computed from. Bucket-scope rows (`/<bucket>` exactly) and
// IAM rows (`iam:...`) return ''.
function resourceToKey(resource: string, bucket: string): string {
  const prefix = `/${bucket}/`;
  if (resource.startsWith(prefix)) return resource.slice(prefix.length);
  return '';
}

interface Props {
  bucket: BucketDetail;
}

interface DrillState {
  shard: string;
  cellStartTs: number;
  cellEndTs: number;
}

export function BucketHotShardsTab({ bucket }: Props) {
  const [rangeKey, setRangeKey] = useState<RangeKey>('1h');
  const [drill, setDrill] = useState<DrillState | null>(null);
  const opt = useMemo(
    () => RANGE_OPTIONS.find((r) => r.key === rangeKey) ?? RANGE_OPTIONS[1],
    [rangeKey],
  );

  const q = useQuery({
    queryKey: queryKeys.diagnostics.hotShards(bucket.name, opt.range, opt.step),
    queryFn: () =>
      fetchHotShards({ bucket: bucket.name, range: opt.range, step: opt.step }),
    refetchInterval: POLL_INTERVAL_MS,
    placeholderData: keepPreviousData,
    retry: (failureCount, err) => {
      if (isMetricsUnavailable(err)) return false;
      return failureCount < 1;
    },
    meta: { label: 'hot shards', silent: true },
  });

  const matrix = q.data?.matrix ?? [];
  const rows = useMemo(() => toHeatmapRows(matrix), [matrix]);
  const promUnavailable = isMetricsUnavailable(q.error);
  const empty = q.data?.empty ?? false;
  const reason = q.data?.reason ?? '';
  const showSkeleton = q.isPending && !q.data && !promUnavailable;
  const otherError =
    !q.data && !promUnavailable && q.error instanceof Error ? q.error.message : null;

  function handleRefresh() {
    void queryClient.invalidateQueries({
      queryKey: ['diagnostics', 'hot-shards', bucket.name],
    });
  }

  function handleCellClick(c: HeatmapClick) {
    // Heatmap rows label as `shard N`; recover the raw shard id.
    const shard = c.label.replace(/^shard\s+/, '');
    setDrill({ shard, cellStartTs: c.cellStartTs, cellEndTs: c.cellEndTs });
  }

  // s3-over-s3 backend: explainer card — no shards, no heatmap.
  if (empty) {
    return (
      <Card className="border-amber-500/40 bg-amber-500/5">
        <CardContent className="flex items-start gap-3 py-4 text-sm text-amber-900 dark:text-amber-200">
          <Info className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
          <div className="space-y-1">
            <div className="font-medium">Shard heatmap is not applicable</div>
            <p>
              Shard heatmap is meaningful only for RADOS-backed clusters. The
              s3-over-s3 backend stores each Strata object as one backend
              object — no shards.
            </p>
            {reason && (
              <p className="font-mono text-xs text-amber-800/80 dark:text-amber-200/70">
                {reason}
              </p>
            )}
          </div>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-4">
      {promUnavailable && (
        <Card className="border-amber-500/40 bg-amber-500/5">
          <CardContent className="flex items-start gap-3 py-4 text-sm text-amber-900 dark:text-amber-200">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div className="space-y-1">
              <div className="font-medium">Metrics unavailable</div>
              <p>
                Hot shards reads from Prometheus. Set{' '}
                <code className="font-mono">STRATA_PROMETHEUS_URL</code> on the
                gateway and restart to enable this view.
              </p>
              <a
                href="https://danchupin.github.io/strata/best-practices/monitoring/#prometheus"
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-amber-900 underline-offset-2 hover:underline dark:text-amber-100"
              >
                Monitoring guide — Prometheus setup
                <ExternalLink className="h-3 w-3" aria-hidden />
              </a>
            </div>
          </CardContent>
        </Card>
      )}

      {otherError && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="py-4 text-sm text-destructive">
            <div className="font-medium">Failed to load hot shards</div>
            <div className="text-xs text-destructive/80">{otherError}</div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
          <div className="space-y-1">
            <CardTitle className="text-base">LWT-conflict rate per shard</CardTitle>
            <CardDescription>
              {bucket.shard_count} shards · range {opt.label} · step {opt.step}
              {' '}· auto-refresh 30 s · click a cell to drill into top
              contended keys.
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <div
              role="group"
              aria-label="Range"
              className="inline-flex rounded-md border border-input bg-background p-0.5 text-sm"
            >
              {RANGE_OPTIONS.map((o) => (
                <button
                  key={o.key}
                  type="button"
                  aria-pressed={rangeKey === o.key}
                  onClick={() => setRangeKey(o.key)}
                  className={cn(
                    'rounded-sm px-2.5 py-1 font-medium transition-colors',
                    rangeKey === o.key
                      ? 'bg-primary text-primary-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  {o.label}
                </button>
              ))}
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching || promUnavailable}
              aria-label="Refresh hot shards"
            >
              <RefreshCw
                className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
                aria-hidden
              />
              Refresh
            </Button>
          </div>
        </CardHeader>
        <CardContent>
          {promUnavailable ? (
            <div className="flex h-56 items-center justify-center rounded-md border border-dashed border-border/60 bg-muted/30 text-sm text-muted-foreground">
              Heatmap disabled until Prometheus is configured.
            </div>
          ) : showSkeleton ? (
            <Skeleton className="h-72 w-full" />
          ) : (
            <Heatmap rows={rows} onCellClick={handleCellClick} />
          )}
        </CardContent>
      </Card>

      <ContendedKeysSheet
        bucket={bucket}
        drill={drill}
        onClose={() => setDrill(null)}
      />
    </div>
  );
}

interface ContendedKeysSheetProps {
  bucket: BucketDetail;
  drill: DrillState | null;
  onClose: () => void;
}

interface ContendedRow {
  key: string;
  hits: number;
  truncated: boolean;
}

function ContendedKeysSheet({ bucket, drill, onClose }: ContendedKeysSheetProps) {
  const open = drill != null;
  const since = drill ? new Date(drill.cellStartTs).toISOString() : '';
  const until = drill ? new Date(drill.cellEndTs).toISOString() : '';

  const auditQ = useQuery({
    queryKey: ['diagnostics', 'hot-shards', 'drill', bucket.name, drill?.shard, since, until],
    queryFn: () =>
      fetchAuditLog({
        bucket: bucket.name,
        since,
        until,
        limit: 500,
      }),
    enabled: open,
  });

  const targetShard = drill ? Number(drill.shard) : -1;
  const shardCount = bucket.shard_count;
  const records = auditQ.data?.records ?? [];

  const contended = useMemo<ContendedRow[]>(() => {
    if (!open || !Number.isFinite(targetShard) || shardCount <= 0) return [];
    const tally = new Map<string, ContendedRow>();
    for (const r of records as AuditRecord[]) {
      const key = resourceToKey(r.resource, bucket.name);
      if (!key) continue;
      if (shardOf(key, shardCount) !== targetShard) continue;
      const existing = tally.get(key);
      if (existing) {
        existing.hits++;
      } else {
        tally.set(key, {
          key,
          hits: 1,
          // Audit middleware stores `resource = "/<bucket>/<key>"` and
          // Cassandra clamps that column to 200 chars; flag for tooltip.
          truncated: r.resource.length >= AUDIT_RESOURCE_TRUNCATE_LEN,
        });
      }
    }
    return Array.from(tally.values()).sort((a, b) => b.hits - a.hits);
  }, [open, records, bucket.name, shardCount, targetShard]);

  return (
    <Sheet open={open} onOpenChange={(o) => (!o ? onClose() : undefined)}>
      <SheetContent
        side="right"
        className="w-full overflow-y-auto sm:max-w-lg"
        aria-describedby="hot-shards-drill-desc"
      >
        <SheetHeader>
          <SheetTitle>
            Top contended keys · shard {drill?.shard ?? ''}
          </SheetTitle>
          <SheetDescription id="hot-shards-drill-desc">
            {drill ? (
              <>
                {new Date(drill.cellStartTs).toLocaleString()} →{' '}
                {new Date(drill.cellEndTs).toLocaleString()}
                <br />
                Audit-log rows with key whose <code>fnv1a32(key) %{' '}
                {shardCount}</code> equals this shard.
              </>
            ) : null}
          </SheetDescription>
        </SheetHeader>
        <div className="mt-6 space-y-3 text-sm">
          {auditQ.isPending && (
            <div className="space-y-2">
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-2/3" />
            </div>
          )}
          {!auditQ.isPending && contended.length === 0 && (
            <div className="rounded-md border border-dashed border-border/60 bg-muted/30 p-4 text-muted-foreground">
              No audit rows in this timespan map to shard {drill?.shard}. The
              conflict counter saw events but the audit log may have aged out
              or been filtered by retention.
            </div>
          )}
          {!auditQ.isPending && contended.length > 0 && (
            <table className="w-full table-auto text-left text-xs">
              <thead className="text-muted-foreground">
                <tr>
                  <th className="py-1 pr-2">Key</th>
                  <th className="py-1 text-right">Hits</th>
                </tr>
              </thead>
              <tbody>
                {contended.slice(0, 100).map((row) => (
                  <tr
                    key={row.key}
                    className="border-t border-border/60 align-top"
                  >
                    <td className="py-1 pr-2 font-mono">
                      <span
                        className={cn(row.truncated && 'underline decoration-dotted')}
                        title={
                          row.truncated
                            ? `Audit resource truncated at ${AUDIT_RESOURCE_TRUNCATE_LEN} chars (Cassandra schema limit).`
                            : row.key
                        }
                      >
                        {row.key}
                      </span>
                    </td>
                    <td className="py-1 text-right tabular-nums">{row.hits}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
