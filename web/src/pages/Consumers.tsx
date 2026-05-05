import { useState } from 'react';
import { useQuery, keepPreviousData } from '@tanstack/react-query';
import { AlertCircle, RefreshCw } from 'lucide-react';

import { fetchTopConsumers, type ConsumersTopBy } from '@/api/widgets';
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
import { cn } from '@/lib/utils';

const POLL_MS = 30_000;
const LIMITS = [10, 50, 100] as const;

type Limit = (typeof LIMITS)[number];

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

export function ConsumersPage() {
  const [by, setBy] = useState<ConsumersTopBy>('requests');
  const [limit, setLimit] = useState<Limit>(50);

  const { data, isLoading, isFetching, refetch, error } = useQuery({
    queryKey: ['consumers', 'top', by, limit] as const,
    queryFn: () => fetchTopConsumers(by, limit),
    refetchInterval: POLL_MS,
    placeholderData: keepPreviousData,
  });

  const consumers = data?.consumers ?? [];
  const metricsAvailable = data?.metrics_available ?? false;

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Consumers</h1>
          <p className="text-sm text-muted-foreground">
            Top access keys by request count and bytes over the last 24 h.
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={() => refetch()}
          disabled={isFetching}
          aria-label="Refresh"
        >
          <RefreshCw
            className={cn('mr-1 h-4 w-4', isFetching && 'animate-spin')}
          />
          Refresh
        </Button>
      </div>

      <Card>
        <CardHeader className="flex-row flex-wrap items-end justify-between gap-3 space-y-0">
          <div className="flex flex-wrap items-end gap-3">
            <div className="space-y-1">
              <label className="text-xs font-medium text-muted-foreground">
                Sort by
              </label>
              <div
                role="tablist"
                aria-label="Sort by"
                className="inline-flex items-center rounded-md border bg-muted/40 p-0.5"
              >
                {(['requests', 'bytes'] as const).map((opt) => (
                  <button
                    key={opt}
                    type="button"
                    role="tab"
                    aria-selected={by === opt}
                    onClick={() => setBy(opt)}
                    className={cn(
                      'rounded-sm px-3 py-1 text-sm font-medium capitalize transition-colors',
                      by === opt
                        ? 'bg-background text-foreground shadow-sm'
                        : 'text-muted-foreground hover:text-foreground',
                    )}
                  >
                    {opt}
                  </button>
                ))}
              </div>
            </div>
            <div className="space-y-1">
              <label className="text-xs font-medium text-muted-foreground">
                Limit
              </label>
              <div
                role="tablist"
                aria-label="Limit"
                className="inline-flex items-center rounded-md border bg-muted/40 p-0.5"
              >
                {LIMITS.map((opt) => (
                  <button
                    key={opt}
                    type="button"
                    role="tab"
                    aria-selected={limit === opt}
                    onClick={() => setLimit(opt)}
                    className={cn(
                      'rounded-sm px-3 py-1 text-sm font-medium transition-colors',
                      limit === opt
                        ? 'bg-background text-foreground shadow-sm'
                        : 'text-muted-foreground hover:text-foreground',
                    )}
                  >
                    {opt}
                  </button>
                ))}
              </div>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          {error ? (
            <ConsumersError message={String((error as Error).message)} />
          ) : isLoading && consumers.length === 0 ? (
            <ConsumersSkeleton rows={Math.min(limit, 10)} />
          ) : !metricsAvailable && consumers.length === 0 ? (
            <ConsumersMetricsUnavailable />
          ) : consumers.length === 0 ? (
            <ConsumersEmpty />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[2.5rem]">#</TableHead>
                  <TableHead>Access key</TableHead>
                  <TableHead>User</TableHead>
                  <TableHead className="text-right">Requests (24 h)</TableHead>
                  <TableHead className="text-right">Bytes (24 h)</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {consumers.map((c, idx) => (
                  <TableRow key={c.access_key || idx}>
                    <TableCell className="text-muted-foreground">
                      {idx + 1}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {c.access_key || '—'}
                    </TableCell>
                    <TableCell>{c.user || '—'}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatCount(c.request_count_24h)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatBytes(c.bytes_24h)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function ConsumersError({ message }: { message: string }) {
  return (
    <div className="flex items-start gap-3 rounded-md border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">
      <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
      <div>
        <div className="font-medium">Failed to load consumers</div>
        <div className="text-xs opacity-80">{message}</div>
      </div>
    </div>
  );
}

function ConsumersMetricsUnavailable() {
  return (
    <Card className="border-dashed">
      <CardHeader>
        <CardTitle className="text-base">Metrics unavailable</CardTitle>
        <CardDescription>
          Top consumers are derived from PromQL over
          <code className="mx-1 rounded bg-muted px-1 text-xs">
            strata_http_requests_total
          </code>
          + per-access-key labels. The gateway has no
          <code className="mx-1 rounded bg-muted px-1 text-xs">
            STRATA_PROMETHEUS_URL
          </code>
          configured, so this page cannot populate.
        </CardDescription>
      </CardHeader>
      <CardContent className="text-sm text-muted-foreground">
        Set
        <code className="mx-1 rounded bg-muted px-1 text-xs">
          STRATA_PROMETHEUS_URL
        </code>
        on the gateway and re-deploy. See{' '}
        <a
          href="/console/docs/ui#prometheus-setup"
          className="underline underline-offset-2"
        >
          docs/ui.md
        </a>{' '}
        for the operator-side setup.
      </CardContent>
    </Card>
  );
}

function ConsumersEmpty() {
  return (
    <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
      No traffic in the last 24 h.
    </div>
  );
}

function ConsumersSkeleton({ rows }: { rows: number }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead className="w-[2.5rem]">#</TableHead>
          <TableHead>Access key</TableHead>
          <TableHead>User</TableHead>
          <TableHead className="text-right">Requests (24 h)</TableHead>
          <TableHead className="text-right">Bytes (24 h)</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {Array.from({ length: rows }).map((_, i) => (
          <TableRow key={i}>
            <TableCell>
              <Skeleton className="h-4 w-4" />
            </TableCell>
            <TableCell>
              <Skeleton className="h-4 w-40" />
            </TableCell>
            <TableCell>
              <Skeleton className="h-4 w-24" />
            </TableCell>
            <TableCell className="text-right">
              <Skeleton className="ml-auto h-4 w-16" />
            </TableCell>
            <TableCell className="text-right">
              <Skeleton className="ml-auto h-4 w-20" />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
