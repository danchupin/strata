import { useMemo } from 'react';
import { keepPreviousData, useIsFetching, useQuery } from '@tanstack/react-query';
import { AlertCircle, AlertTriangle, RefreshCw } from 'lucide-react';
import {
  Bar,
  BarChart,
  CartesianGrid,
  Legend,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';

import {
  fetchStorageClasses,
  fetchStorageData,
  fetchStorageMeta,
  type DataHealthReport,
  type MetaHealthReport,
  type NodeStatus,
  type PoolStatus,
  type StorageClassesResponse,
} from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { ClustersSubsection } from '@/components/storage/ClustersSubsection';
import { queryClient, queryKeys } from '@/lib/query';
import { cn } from '@/lib/utils';

const POLL_MS = 30_000;

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
  return Math.round(n).toLocaleString();
}

function backendLabel(b: string): string {
  if (!b) return 'Unknown';
  return b.charAt(0).toUpperCase() + b.slice(1);
}

function stateVariant(s: string | undefined): {
  className: string;
} {
  switch ((s ?? '').toLowerCase()) {
    case 'up':
    case 'reachable':
    case 'self':
    case 'in-process':
      return {
        className:
          'bg-emerald-500/15 text-emerald-700 border-emerald-500/30 dark:text-emerald-300',
      };
    case 'down':
    case 'error':
    case 'offline':
    case 'unreachable':
      return {
        className:
          'bg-red-500/15 text-red-700 border-red-500/30 dark:text-red-300',
      };
    case 'tombstone':
    case 'leaving':
    case 'joining':
      return {
        className:
          'bg-amber-500/15 text-amber-800 border-amber-500/30 dark:text-amber-300',
      };
    default:
      return { className: 'bg-muted text-muted-foreground border-border' };
  }
}

function StateBadge({ state }: { state: string }) {
  const v = stateVariant(state);
  return (
    <Badge variant="outline" className={cn('font-medium', v.className)}>
      {state || '—'}
    </Badge>
  );
}

function WarningBanner({ warnings }: { warnings: string[] }) {
  if (!warnings || warnings.length === 0) return null;
  return (
    <Card className="border-amber-500/40 bg-amber-500/5">
      <CardContent className="flex items-start gap-3 py-4 text-sm text-amber-900 dark:text-amber-200">
        <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
        <div className="space-y-1">
          <div className="font-medium">
            {warnings.length === 1
              ? '1 warning reported by backend'
              : `${warnings.length} warnings reported by backend`}
          </div>
          <ul className="list-inside list-disc space-y-0.5 text-xs">
            {warnings.map((w, i) => (
              <li key={`${i}-${w}`}>{w}</li>
            ))}
          </ul>
        </div>
      </CardContent>
    </Card>
  );
}

function MemoryExplainer({ kind }: { kind: 'meta' | 'data' }) {
  return (
    <Card className="border-dashed">
      <CardContent className="py-4 text-sm text-muted-foreground">
        Backend is{' '}
        <code className="rounded bg-muted px-1 text-xs">memory</code> — single
        in-process row, no network topology to report. Configure{' '}
        <code className="rounded bg-muted px-1 text-xs">
          {kind === 'meta'
            ? 'STRATA_META_BACKEND=cassandra | tikv'
            : 'STRATA_DATA_BACKEND=rados | s3'}
        </code>{' '}
        to surface real {kind === 'meta' ? 'peers' : 'pools'}.
      </CardContent>
    </Card>
  );
}

function ErrorCard({ message }: { message: string }) {
  return (
    <Card className="border-destructive/40 bg-destructive/5">
      <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
        <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
        <div>
          <div className="font-medium">Failed to load</div>
          <div className="text-xs text-destructive/80">{message}</div>
        </div>
      </CardContent>
    </Card>
  );
}

function MetaTab() {
  const q = useQuery({
    queryKey: queryKeys.storage.meta,
    queryFn: fetchStorageMeta,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'storage meta' },
  });

  const report: MetaHealthReport | undefined = q.data;
  const showSkeleton = q.isPending && !q.data;
  const errMsg = !q.data && q.error instanceof Error ? q.error.message : null;
  const nodes: NodeStatus[] = report?.nodes ?? [];

  return (
    <div className="space-y-4">
      {errMsg && <ErrorCard message={errMsg} />}
      {report && <WarningBanner warnings={report.warnings ?? []} />}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            Backend ={' '}
            <span className="font-mono">
              {report ? backendLabel(report.backend) : '—'}
            </span>
          </CardTitle>
          <CardDescription>
            {report
              ? `Replication factor ${report.replication_factor} · ${nodes.length} ${
                  nodes.length === 1 ? 'node' : 'nodes'
                }`
              : 'Loading meta backend topology…'}
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          {showSkeleton ? (
            <div className="space-y-2 px-4 sm:px-6">
              {Array.from({ length: 3 }).map((_, i) => (
                <Skeleton key={i} className="h-9 w-full" />
              ))}
            </div>
          ) : report?.backend === 'memory' ? (
            <div className="px-4 sm:px-6">
              <MemoryExplainer kind="meta" />
            </div>
          ) : nodes.length === 0 ? (
            <div className="px-4 text-sm text-muted-foreground sm:px-6">
              No nodes reported.
            </div>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="pl-4 sm:pl-6">Address</TableHead>
                    <TableHead>State</TableHead>
                    <TableHead>DC</TableHead>
                    <TableHead>Rack</TableHead>
                    <TableHead className="pr-4 sm:pr-6">
                      Schema version
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {nodes.map((n) => (
                    <TableRow key={`${n.address}-${n.schema_version ?? ''}`}>
                      <TableCell className="pl-4 font-mono text-xs sm:pl-6">
                        {n.address || '—'}
                      </TableCell>
                      <TableCell>
                        <StateBadge state={n.state} />
                      </TableCell>
                      <TableCell>{n.data_center || '—'}</TableCell>
                      <TableCell>{n.rack || '—'}</TableCell>
                      <TableCell className="pr-4 font-mono text-xs sm:pr-6">
                        {n.schema_version || '—'}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

const CLASS_COLORS = [
  'hsl(220 90% 56%)',
  'hsl(160 70% 40%)',
  'hsl(30 90% 55%)',
  'hsl(280 70% 55%)',
  'hsl(340 80% 55%)',
  'hsl(200 80% 50%)',
  'hsl(100 60% 45%)',
];

interface StackRow {
  name: 'total';
  [klass: string]: string | number;
}

function StorageClassesCard({
  data,
  loading,
  errorMsg,
}: {
  data: StorageClassesResponse | undefined;
  loading: boolean;
  errorMsg: string | null;
}) {
  const classes = data?.classes ?? [];
  const pools = data?.pools_by_class ?? {};
  const totalBytes = useMemo(
    () => classes.reduce((acc, c) => acc + c.bytes, 0),
    [classes],
  );
  const stackRow = useMemo<StackRow>(() => {
    const row: StackRow = { name: 'total' };
    for (const c of classes) row[c.class] = c.bytes;
    return row;
  }, [classes]);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Storage classes</CardTitle>
        <CardDescription>
          Cluster-wide bytes + objects per storage class · sampled by the
          bucketstats worker.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {errorMsg && <ErrorCard message={errorMsg} />}
        {loading ? (
          <Skeleton className="h-32 w-full" />
        ) : classes.length === 0 ? (
          <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
            No per-class breakdown yet — the bucketstats sampler runs once per
            cycle and seeds this view on the next pass.
          </div>
        ) : (
          <>
            <div className="text-xs uppercase tracking-wide text-muted-foreground">
              Total {formatBytes(totalBytes)} across {classes.length}{' '}
              {classes.length === 1 ? 'class' : 'classes'}
            </div>
            <div className="h-24 w-full" aria-label="Bytes per storage class">
              <ResponsiveContainer width="100%" height="100%">
                <BarChart
                  data={[stackRow]}
                  layout="vertical"
                  margin={{ top: 4, right: 16, bottom: 4, left: 0 }}
                >
                  <CartesianGrid
                    strokeDasharray="3 3"
                    horizontal={false}
                    className="stroke-border/40"
                  />
                  <XAxis
                    type="number"
                    tickFormatter={formatBytes}
                    stroke="currentColor"
                    className="text-xs text-muted-foreground"
                  />
                  <YAxis type="category" dataKey="name" hide />
                  <Tooltip
                    formatter={(v: number, name: string) => [
                      formatBytes(v),
                      name,
                    ]}
                  />
                  <Legend />
                  {classes.map((c, i) => (
                    <Bar
                      key={c.class}
                      dataKey={c.class}
                      stackId="a"
                      fill={CLASS_COLORS[i % CLASS_COLORS.length]}
                      isAnimationActive={false}
                    />
                  ))}
                </BarChart>
              </ResponsiveContainer>
            </div>
            <div className="flex flex-wrap gap-2">
              {classes.map((c, i) => {
                const pct =
                  totalBytes > 0 ? (c.bytes / totalBytes) * 100 : 0;
                const pool = pools[c.class];
                return (
                  <Badge
                    key={c.class}
                    variant="outline"
                    className="gap-1.5 font-normal"
                  >
                    <span
                      aria-hidden
                      className="h-2 w-2 rounded-full"
                      style={{
                        backgroundColor:
                          CLASS_COLORS[i % CLASS_COLORS.length],
                      }}
                    />
                    <span className="font-medium">{c.class}</span>
                    <span className="text-muted-foreground">
                      {formatBytes(c.bytes)} · {formatCount(c.objects)} obj
                      {pct > 0 && ` · ${pct.toFixed(1)}%`}
                    </span>
                    {pool && (
                      <span className="font-mono text-[10px] text-muted-foreground">
                        → {pool}
                      </span>
                    )}
                  </Badge>
                );
              })}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}

function DataTab() {
  const dataQ = useQuery({
    queryKey: queryKeys.storage.data,
    queryFn: fetchStorageData,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'storage data' },
  });
  const classesQ = useQuery({
    queryKey: queryKeys.storage.classes,
    queryFn: fetchStorageClasses,
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'storage classes' },
  });

  const report: DataHealthReport | undefined = dataQ.data;
  const showSkeleton = dataQ.isPending && !dataQ.data;
  const errMsg =
    !dataQ.data && dataQ.error instanceof Error ? dataQ.error.message : null;
  const classesErr =
    !classesQ.data && classesQ.error instanceof Error
      ? classesQ.error.message
      : null;
  const pools: PoolStatus[] = report?.pools ?? [];

  // Stable sort on Cluster column with empty/undefined sorting last.
  // The wire response from rados/health.go is already grouped by
  // (cluster, pool, ns), but a generic ascending sort here makes the
  // contract self-evident at the call site and tolerates a future
  // backend that doesn't pre-sort.
  const sortedPools = useMemo(() => {
    const arr = pools.slice();
    arr.sort((a, b) => {
      const ac = a.cluster ?? '';
      const bc = b.cluster ?? '';
      if (ac === bc) return 0;
      if (ac === '') return 1;
      if (bc === '') return -1;
      return ac.localeCompare(bc);
    });
    return arr;
  }, [pools]);
  const isMemory = report?.backend === 'memory';

  return (
    <div className="space-y-4">
      {errMsg && <ErrorCard message={errMsg} />}
      {report && <WarningBanner warnings={report.warnings ?? []} />}

      {!isMemory && <ClustersSubsection pools={sortedPools} />}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            Backend ={' '}
            <span className="font-mono">
              {report ? backendLabel(report.backend) : '—'}
            </span>
          </CardTitle>
          <CardDescription>
            {report
              ? `${pools.length} ${pools.length === 1 ? 'pool' : 'pools'}`
              : 'Loading data backend pool topology…'}
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          {showSkeleton ? (
            <div className="space-y-2 px-4 sm:px-6">
              {Array.from({ length: 2 }).map((_, i) => (
                <Skeleton key={i} className="h-9 w-full" />
              ))}
            </div>
          ) : isMemory ? (
            <div className="px-4 sm:px-6">
              <MemoryExplainer kind="data" />
            </div>
          ) : sortedPools.length === 0 ? (
            <div className="px-4 text-sm text-muted-foreground sm:px-6">
              No pools reported.
            </div>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="pl-4 sm:pl-6">Name</TableHead>
                    <TableHead>Cluster</TableHead>
                    <TableHead>Class</TableHead>
                    <TableHead className="text-right">Bytes used</TableHead>
                    <TableHead
                      className="text-right"
                      title="RADOS chunk count — large S3 objects span multiple 4 MiB chunks. For S3 object count see BucketDetail."
                    >
                      Chunks
                    </TableHead>
                    <TableHead className="text-right">Replicas</TableHead>
                    <TableHead className="pr-4 sm:pr-6">State</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {sortedPools.map((p, idx) => (
                    <TableRow key={`${p.cluster ?? ''}-${p.name}-${p.class}-${idx}`}>
                      <TableCell className="pl-4 font-mono text-xs sm:pl-6">
                        {p.name || '—'}
                      </TableCell>
                      <TableCell className="font-mono text-xs">
                        {p.cluster ? (
                          p.cluster
                        ) : (
                          <span className="text-muted-foreground">—</span>
                        )}
                      </TableCell>
                      <TableCell className="font-mono text-xs">
                        {p.class || '—'}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatBytes(p.bytes_used)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatCount(p.chunk_count)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {p.num_replicas || '—'}
                      </TableCell>
                      <TableCell className="pr-4 sm:pr-6">
                        <StateBadge state={p.state} />
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      <StorageClassesCard
        data={classesQ.data}
        loading={classesQ.isPending && !classesQ.data}
        errorMsg={classesErr}
      />
    </div>
  );
}

export function StoragePage() {
  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: queryKeys.storage.meta });
    void queryClient.invalidateQueries({ queryKey: queryKeys.storage.data });
    void queryClient.invalidateQueries({
      queryKey: queryKeys.storage.classes,
    });
  }

  const fetchingCount = useIsFetching({ predicate: (q) => {
    const k = q.queryKey;
    return Array.isArray(k) && k[0] === 'storage';
  } });
  const isFetching = fetchingCount > 0;

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Storage</h1>
          <p className="text-sm text-muted-foreground">
            Meta + data backend health and per-storage-class breakdown.
            Auto-refresh 30 s.
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={handleRefresh}
          disabled={isFetching}
          aria-label="Refresh"
        >
          <RefreshCw
            className={cn('mr-1 h-4 w-4', isFetching && 'animate-spin')}
          />
          Refresh
        </Button>
      </div>

      <Tabs defaultValue="meta" className="w-full">
        <TabsList>
          <TabsTrigger value="meta">Meta</TabsTrigger>
          <TabsTrigger value="data">Data</TabsTrigger>
        </TabsList>
        <TabsContent value="meta" className="mt-4">
          <MetaTab />
        </TabsContent>
        <TabsContent value="data" className="mt-4">
          <DataTab />
        </TabsContent>
      </Tabs>
    </div>
  );
}

export default StoragePage;
