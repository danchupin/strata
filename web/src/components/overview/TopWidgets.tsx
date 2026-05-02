import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { AlertTriangle } from 'lucide-react';

import {
  fetchTopBuckets,
  fetchTopConsumers,
  type BucketTop,
  type BucketsTopBy,
  type ConsumerTop,
  type ConsumersTopBy,
} from '@/api/widgets';
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

const POLL_INTERVAL_MS = 5_000;

interface WidgetState<T> {
  rows: T[];
  metricsAvailable: boolean;
  loading: boolean;
  error: string | null;
}

function useTopBuckets(by: BucketsTopBy) {
  const [state, setState] = useState<WidgetState<BucketTop>>({
    rows: [],
    metricsAvailable: false,
    loading: true,
    error: null,
  });
  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const body = await fetchTopBuckets(by);
        if (cancelled) return;
        setState({
          rows: body.buckets,
          metricsAvailable: body.metrics_available,
          loading: false,
          error: null,
        });
      } catch (e) {
        if (cancelled) return;
        setState((prev) => ({
          ...prev,
          loading: false,
          error: e instanceof Error ? e.message : 'load failed',
        }));
      }
    }
    setState((prev) => ({ ...prev, loading: true }));
    load();
    const id = window.setInterval(load, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [by]);
  return state;
}

function useTopConsumers(by: ConsumersTopBy) {
  const [state, setState] = useState<WidgetState<ConsumerTop>>({
    rows: [],
    metricsAvailable: false,
    loading: true,
    error: null,
  });
  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const body = await fetchTopConsumers(by);
        if (cancelled) return;
        setState({
          rows: body.consumers,
          metricsAvailable: body.metrics_available,
          loading: false,
          error: null,
        });
      } catch (e) {
        if (cancelled) return;
        setState((prev) => ({
          ...prev,
          loading: false,
          error: e instanceof Error ? e.message : 'load failed',
        }));
      }
    }
    setState((prev) => ({ ...prev, loading: true }));
    load();
    const id = window.setInterval(load, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [by]);
  return state;
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

function truncateAccessKey(ak: string): string {
  if (!ak) return '—';
  return ak.length <= 8 ? ak : ak.slice(0, 8) + '…';
}

function MetricsUnavailable() {
  return (
    <div className="flex items-start gap-1.5 text-xs text-amber-700 dark:text-amber-300">
      <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
      <span>Metrics unavailable — set STRATA_PROMETHEUS_URL to enable.</span>
    </div>
  );
}

function EmptyRow({ cols, label }: { cols: number; label: string }) {
  return (
    <TableRow>
      <TableCell
        colSpan={cols}
        className="py-4 text-center text-xs text-muted-foreground"
      >
        {label}
      </TableCell>
    </TableRow>
  );
}

function LoadingRows({ cols, count = 3 }: { cols: number; count?: number }) {
  return (
    <>
      {Array.from({ length: count }).map((_, i) => (
        <TableRow key={i}>
          <TableCell colSpan={cols} className="py-2">
            <Skeleton className="h-4 w-full" />
          </TableCell>
        </TableRow>
      ))}
    </>
  );
}

interface BucketTabProps {
  by: BucketsTopBy;
  valueLabel: string;
}

function BucketTab({ by, valueLabel }: BucketTabProps) {
  const { rows, metricsAvailable, loading, error } = useTopBuckets(by);
  const showMetricsWarning = by === 'requests' && !metricsAvailable && !loading;

  return (
    <div className="space-y-2">
      {showMetricsWarning && <MetricsUnavailable />}
      {error && (
        <div className="text-xs text-destructive">Failed: {error}</div>
      )}
      <div className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-12 pl-4 sm:pl-6">#</TableHead>
              <TableHead>Bucket</TableHead>
              <TableHead className="text-right pr-4 sm:pr-6">{valueLabel}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading && rows.length === 0 && <LoadingRows cols={3} />}
            {!loading && rows.length === 0 && (
              <EmptyRow cols={3} label="No buckets" />
            )}
            {rows.map((b, idx) => {
              const value =
                by === 'size'
                  ? formatBytes(b.size_bytes)
                  : metricsAvailable
                  ? formatCount(b.request_count_24h)
                  : '—';
              return (
                <TableRow key={b.name}>
                  <TableCell className="pl-4 text-muted-foreground sm:pl-6">
                    {idx + 1}
                  </TableCell>
                  <TableCell className="font-medium">
                    <Link
                      to={`/buckets/${encodeURIComponent(b.name)}`}
                      className="hover:underline"
                    >
                      {b.name}
                    </Link>
                  </TableCell>
                  <TableCell className="text-right pr-4 font-mono text-xs sm:pr-6">
                    {value}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

export function TopBucketsCard() {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Top Buckets</CardTitle>
        <CardDescription>
          Top 10 buckets by stored size or 24-hour request count.
        </CardDescription>
      </CardHeader>
      <CardContent className="px-0 sm:px-0">
        <div className="px-4 sm:px-6">
          <Tabs defaultValue="size">
            <TabsList>
              <TabsTrigger value="size">By Size</TabsTrigger>
              <TabsTrigger value="requests">By Requests</TabsTrigger>
            </TabsList>
            <TabsContent value="size" className="mt-4">
              <BucketTab by="size" valueLabel="Size" />
            </TabsContent>
            <TabsContent value="requests" className="mt-4">
              <BucketTab by="requests" valueLabel="Requests (24h)" />
            </TabsContent>
          </Tabs>
        </div>
      </CardContent>
    </Card>
  );
}

interface ConsumerTabProps {
  by: ConsumersTopBy;
  valueLabel: string;
}

function ConsumerTab({ by, valueLabel }: ConsumerTabProps) {
  const { rows, metricsAvailable, loading, error } = useTopConsumers(by);
  const showMetricsWarning = !metricsAvailable && !loading;

  return (
    <div className="space-y-2">
      {showMetricsWarning && <MetricsUnavailable />}
      {error && (
        <div className="text-xs text-destructive">Failed: {error}</div>
      )}
      <div className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-12 pl-4 sm:pl-6">#</TableHead>
              <TableHead>Access Key</TableHead>
              <TableHead>User</TableHead>
              <TableHead className="text-right pr-4 sm:pr-6">{valueLabel}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading && rows.length === 0 && <LoadingRows cols={4} />}
            {!loading && rows.length === 0 && (
              <EmptyRow cols={4} label={metricsAvailable ? 'No consumer activity in last 24h' : 'No data'} />
            )}
            {rows.map((c, idx) => {
              const value =
                by === 'requests'
                  ? metricsAvailable
                    ? formatCount(c.request_count_24h)
                    : '—'
                  : metricsAvailable
                  ? formatBytes(c.bytes_24h)
                  : '—';
              return (
                <TableRow key={c.access_key}>
                  <TableCell className="pl-4 text-muted-foreground sm:pl-6">
                    {idx + 1}
                  </TableCell>
                  <TableCell className="font-mono text-xs" title={c.access_key}>
                    {truncateAccessKey(c.access_key)}
                  </TableCell>
                  <TableCell>{c.user || '—'}</TableCell>
                  <TableCell className="text-right pr-4 font-mono text-xs sm:pr-6">
                    {value}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

export function TopConsumersCard() {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Top Consumers</CardTitle>
        <CardDescription>
          Top 10 access keys by 24-hour request count or bytes transferred.
        </CardDescription>
      </CardHeader>
      <CardContent className="px-0 sm:px-0">
        <div className="px-4 sm:px-6">
          <Tabs defaultValue="requests">
            <TabsList>
              <TabsTrigger value="requests">By Requests</TabsTrigger>
              <TabsTrigger value="bytes">By Bytes</TabsTrigger>
            </TabsList>
            <TabsContent value="requests" className="mt-4">
              <ConsumerTab by="requests" valueLabel="Requests (24h)" />
            </TabsContent>
            <TabsContent value="bytes" className="mt-4">
              <ConsumerTab by="bytes" valueLabel="Bytes (24h)" />
            </TabsContent>
          </Tabs>
        </div>
      </CardContent>
    </Card>
  );
}
