import { Suspense, lazy, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, Database, HardDrive, Server } from 'lucide-react';
import { Link } from 'react-router-dom';

import {
  fetchClusterNodes,
  fetchClusterStatus,
  fetchStorageClasses,
  type ClusterNode,
  type ClusterStatus,
  type StorageClassesResponse,
} from '@/api/client';

// NodeDetailDrawer pulls recharts (~110 KiB gz). Lazy-load so the home page
// does not pay the recharts bundle cost until the operator clicks a node row.
const NodeDetailDrawer = lazy(() =>
  import('@/components/NodeDetailDrawer').then((m) => ({
    default: m.NodeDetailDrawer,
  })),
);
import { queryKeys } from '@/lib/query';
import { Badge } from '@/components/ui/badge';
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
import { TopBucketsCard, TopConsumersCard } from '@/components/overview/TopWidgets';
import { cn } from '@/lib/utils';

const HEARTBEAT_INTERVAL_S = 5;

function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—';
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) {
    const dayLabel = days === 1 ? 'day' : 'days';
    const hourLabel = hours === 1 ? 'hour' : 'hours';
    return `${days} ${dayLabel}, ${hours} ${hourLabel}`;
  }
  if (hours > 0) {
    const hourLabel = hours === 1 ? 'hour' : 'hours';
    const minLabel = minutes === 1 ? 'minute' : 'minutes';
    return `${hours} ${hourLabel}, ${minutes} ${minLabel}`;
  }
  return `${minutes}m`;
}

function statusVariant(s: string | undefined): {
  label: string;
  className: string;
} {
  switch (s) {
    case 'healthy':
      return {
        label: 'Healthy',
        className:
          'bg-emerald-500/15 text-emerald-700 border-emerald-500/30 dark:text-emerald-300',
      };
    case 'degraded':
      return {
        label: 'Degraded',
        className:
          'bg-amber-500/15 text-amber-800 border-amber-500/30 dark:text-amber-300',
      };
    case 'unhealthy':
      return {
        label: 'Unhealthy',
        className:
          'bg-red-500/15 text-red-700 border-red-500/30 dark:text-red-300',
      };
    default:
      return {
        label: s ?? 'Unknown',
        className: 'bg-muted text-muted-foreground border-border',
      };
  }
}

// nodeOrder ranks status so unhealthy floats to the top, then degraded, then
// healthy. Within each rank we sort by node id to keep the list stable.
function nodeOrder(n: ClusterNode): number {
  switch (n.status) {
    case 'unhealthy':
      return 0;
    case 'degraded':
      return 1;
    case 'healthy':
      return 2;
    default:
      return 3;
  }
}

function sortedNodes(nodes: ClusterNode[]): ClusterNode[] {
  return [...nodes].sort((a, b) => {
    const oa = nodeOrder(a);
    const ob = nodeOrder(b);
    if (oa !== ob) return oa - ob;
    return a.id.localeCompare(b.id);
  });
}

function StatusBadge({ status }: { status: string | undefined }) {
  const v = statusVariant(status);
  return (
    <Badge variant="outline" className={cn('font-medium', v.className)}>
      {v.label}
    </Badge>
  );
}

function ChipList({ items, empty }: { items: string[]; empty?: string }) {
  if (!items || items.length === 0) {
    return <span className="text-xs text-muted-foreground">{empty ?? '—'}</span>;
  }
  return (
    <div className="flex flex-wrap gap-1">
      {items.map((it) => (
        <Badge key={it} variant="secondary" className="font-normal">
          {it}
        </Badge>
      ))}
    </div>
  );
}

function HeroCard({ status }: { status: ClusterStatus | null }) {
  if (!status) {
    return (
      <Card>
        <CardHeader>
          <Skeleton className="h-6 w-48" />
          <Skeleton className="mt-2 h-4 w-64" />
        </CardHeader>
        <CardContent>
          <Skeleton className="h-16 w-full" />
        </CardContent>
      </Card>
    );
  }
  const { node_count, node_count_healthy } = status;
  const summary =
    node_count === 0
      ? 'No node heartbeats yet'
      : `${node_count_healthy} of ${node_count} ${
          node_count === 1 ? 'node' : 'nodes'
        } healthy`;

  return (
    <Card>
      <CardHeader className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
        <div className="space-y-1">
          <div className="flex items-center gap-2">
            <CardTitle className="text-xl">{status.cluster_name}</CardTitle>
            <StatusBadge status={status.status} />
          </div>
          <CardDescription>{summary}</CardDescription>
        </div>
      </CardHeader>
      <CardContent className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
        <div>
          <div className="text-xs uppercase tracking-wide text-muted-foreground">
            Version
          </div>
          <div className="mt-1 font-mono text-sm">{status.version || '—'}</div>
        </div>
        <div>
          <div className="text-xs uppercase tracking-wide text-muted-foreground">
            Uptime
          </div>
          <div className="mt-1">{formatUptime(status.uptime_sec)}</div>
        </div>
        <div>
          <div className="text-xs uppercase tracking-wide text-muted-foreground">
            Nodes
          </div>
          <div className="mt-1">
            {node_count_healthy} / {node_count}
          </div>
        </div>
        <div>
          <div className="text-xs uppercase tracking-wide text-muted-foreground">
            Started
          </div>
          <div className="mt-1">
            {status.started_at
              ? new Date(status.started_at * 1000).toLocaleString()
              : '—'}
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

function BackendChips({ status }: { status: ClusterStatus | null }) {
  if (!status) return null;
  const meta = status.meta_backend
    ? status.meta_backend.charAt(0).toUpperCase() + status.meta_backend.slice(1)
    : 'Unknown';
  const data = status.data_backend
    ? status.data_backend.charAt(0).toUpperCase() + status.data_backend.slice(1)
    : 'Unknown';
  return (
    <div className="flex flex-wrap gap-2">
      <Badge variant="outline" className="gap-1.5 font-normal">
        <Database className="h-3 w-3" aria-hidden />
        Meta: {meta}
      </Badge>
      <Badge variant="outline" className="gap-1.5 font-normal">
        <Server className="h-3 w-3" aria-hidden />
        Data: {data}
      </Badge>
    </div>
  );
}

// formatBytes is the same helper used on the Storage page; duplicated here so
// the home-page hero can stay free of recharts and other Storage page deps.
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

const STORAGE_HERO_TOPN = 5;

function StorageHeroCard() {
  // 60 s cache per the AC — the hero card is a glance summary, not a live
  // dashboard. The /storage page itself polls every 30 s.
  const q = useQuery({
    queryKey: ['storage', 'classes', 'hero'] as const,
    queryFn: fetchStorageClasses,
    staleTime: 60_000,
    refetchInterval: 60_000,
    meta: { label: 'storage classes', silent: true },
  });
  const data: StorageClassesResponse | undefined = q.data;
  const classes = data?.classes ?? [];
  const total = classes.reduce((acc, c) => acc + c.bytes, 0);
  const top = classes.slice(0, STORAGE_HERO_TOPN);
  const more = classes.length - top.length;

  return (
    <Card>
      <CardHeader className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
        <div className="space-y-1">
          <CardTitle className="flex items-center gap-2 text-base">
            <HardDrive className="h-4 w-4" aria-hidden />
            Storage
          </CardTitle>
          <CardDescription>
            Total {formatBytes(total)} across {classes.length}{' '}
            {classes.length === 1 ? 'class' : 'classes'}
          </CardDescription>
        </div>
        <Link
          to="/storage"
          className="text-xs font-medium text-muted-foreground underline-offset-2 hover:underline"
        >
          View Storage page
        </Link>
      </CardHeader>
      <CardContent>
        {q.isPending && !q.data ? (
          <Skeleton className="h-6 w-full" />
        ) : classes.length === 0 ? (
          <div className="text-sm text-muted-foreground">
            No per-class breakdown yet — bucketstats sampler runs once per
            cycle.
          </div>
        ) : (
          <div className="flex flex-wrap gap-2">
            {top.map((c) => (
              <Badge
                key={c.class}
                variant="outline"
                className="gap-1.5 font-normal"
              >
                <span className="font-medium">{c.class}</span>
                <span className="text-muted-foreground">
                  {formatBytes(c.bytes)}
                </span>
              </Badge>
            ))}
            {more > 0 && (
              <Link
                to="/storage"
                className="inline-flex items-center text-xs font-medium underline underline-offset-2"
              >
                +{more} more
              </Link>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function NodesTable({
  nodes,
  loading,
  onSelect,
}: {
  nodes: ClusterNode[];
  loading: boolean;
  onSelect: (n: ClusterNode) => void;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Nodes</CardTitle>
        <CardDescription>
          Live heartbeats from every Strata replica (10 s write, 30 s TTL).
        </CardDescription>
      </CardHeader>
      <CardContent className="px-0 sm:px-0">
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4 sm:pl-6">Node</TableHead>
                <TableHead>Address</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>Uptime</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Workers</TableHead>
                <TableHead className="pr-4 sm:pr-6">Leader for</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {nodes.length === 0 && !loading && (
                <TableRow>
                  <TableCell
                    colSpan={7}
                    className="py-6 text-center text-sm text-muted-foreground"
                  >
                    No nodes reporting yet.
                  </TableCell>
                </TableRow>
              )}
              {nodes.length === 0 &&
                loading &&
                Array.from({ length: 2 }).map((_, i) => (
                  <TableRow key={i}>
                    <TableCell colSpan={7} className="py-3">
                      <Skeleton className="h-5 w-full" />
                    </TableCell>
                  </TableRow>
                ))}
              {nodes.map((n) => (
                <TableRow
                  key={n.id}
                  className="cursor-pointer hover:bg-muted/50"
                  onClick={() => onSelect(n)}
                  role="button"
                  tabIndex={0}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      onSelect(n);
                    }
                  }}
                >
                  <TableCell className="pl-4 font-medium sm:pl-6">{n.id}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {n.address || '—'}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {n.version || '—'}
                  </TableCell>
                  <TableCell>{formatUptime(n.uptime_sec)}</TableCell>
                  <TableCell>
                    <StatusBadge status={n.status} />
                  </TableCell>
                  <TableCell>
                    <ChipList items={n.workers} />
                  </TableCell>
                  <TableCell className="pr-4 sm:pr-6">
                    <ChipList items={n.leader_for} />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </CardContent>
    </Card>
  );
}

export function OverviewPage() {
  const statusQ = useQuery({
    queryKey: queryKeys.cluster.status,
    queryFn: fetchClusterStatus,
    meta: { label: 'cluster status' },
  });
  const nodesQ = useQuery({
    queryKey: queryKeys.cluster.nodes,
    queryFn: fetchClusterNodes,
    meta: { label: 'cluster nodes' },
  });

  const status: ClusterStatus | null = statusQ.data ?? null;
  const nodes: ClusterNode[] = nodesQ.data ?? [];
  // Show skeletons only while no cached data is available; subsequent refetches
  // keep the existing UI mounted (no blank-out on intermittent network errors).
  const loading = (statusQ.isPending && !statusQ.data) || (nodesQ.isPending && !nodesQ.data);
  // Only render the in-page error banner when there's no usable cached data;
  // transient refetch failures surface via the global toast (web/src/lib/query.ts).
  const errorMessage =
    !statusQ.data && statusQ.error instanceof Error
      ? statusQ.error.message
      : !nodesQ.data && nodesQ.error instanceof Error
      ? nodesQ.error.message
      : null;
  const sorted = sortedNodes(nodes);
  const heartbeatEmpty = !loading && nodes.length === 0;
  const [drilldown, setDrilldown] = useState<ClusterNode | null>(null);

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Cluster Overview</h1>
        <p className="text-sm text-muted-foreground">
          Cluster health, nodes, and top-level activity at a glance.
        </p>
      </div>

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load cluster state</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      <HeroCard status={status} />
      <BackendChips status={status} />
      <StorageHeroCard />

      {heartbeatEmpty && status && (
        <Card className="border-amber-500/40 bg-amber-500/5">
          <CardContent className="flex items-start gap-2 py-3 text-sm text-amber-900 dark:text-amber-200">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              Heartbeat table empty — running single-replica or just started.
              The local node will appear once the heartbeat goroutine ticks
              (every {HEARTBEAT_INTERVAL_S} s).
            </div>
          </CardContent>
        </Card>
      )}

      <NodesTable nodes={sorted} loading={loading} onSelect={setDrilldown} />

      <div className="grid gap-6 xl:grid-cols-2">
        <TopBucketsCard />
        <TopConsumersCard />
      </div>

      {drilldown && (
        <Suspense fallback={null}>
          <NodeDetailDrawer
            node={drilldown}
            open={drilldown !== null}
            onOpenChange={(open) => {
              if (!open) setDrilldown(null);
            }}
            metaBackend={status?.meta_backend}
          />
        </Suspense>
      )}
    </div>
  );
}
