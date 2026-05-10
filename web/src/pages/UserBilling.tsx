import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import {
  AlertCircle,
  ArrowLeft,
  Pencil,
  RefreshCw,
  Trash2,
} from 'lucide-react';
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';

import {
  deleteUserQuota,
  fetchIAMUser,
  fetchUserQuota,
  fetchUserUsage,
  type AdminApiError,
  type UsageRow,
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
import { Skeleton } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { EditUserQuotaDialog } from '@/components/EditUserQuotaDialog';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  let v = n;
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

function usageDayWindow(): { start: string; end: string } {
  const now = new Date();
  const endUTC = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const startUTC = new Date(endUTC.getTime() - 29 * 86400 * 1000);
  const fmt = (d: Date) => d.toISOString().slice(0, 10);
  return { start: fmt(startUTC), end: fmt(endUTC) };
}

export function UserBillingPage() {
  const params = useParams<{ userName: string }>();
  const userName = params.userName ?? '';
  const navigate = useNavigate();
  const range = useMemo(usageDayWindow, []);
  const [editOpen, setEditOpen] = useState(false);

  const userQ = useQuery({
    queryKey: queryKeys.iam.user(userName),
    queryFn: () => fetchIAMUser(userName),
    enabled: !!userName,
    meta: { label: 'iam user' },
    retry: false,
  });

  const quotaQ = useQuery({
    queryKey: queryKeys.iam.userQuota(userName),
    queryFn: () => fetchUserQuota(userName),
    enabled: !!userName,
    meta: { label: 'user quota', silent: true },
    retry: false,
  });

  const usageQ = useQuery({
    queryKey: queryKeys.iam.userUsage(userName, range.start, range.end),
    queryFn: () => fetchUserUsage(userName, range.start, range.end),
    enabled: !!userName,
    placeholderData: keepPreviousData,
    meta: { label: 'user usage' },
  });

  const userMissing =
    userQ.error instanceof Error &&
    'status' in (userQ.error as AdminApiError) &&
    (userQ.error as AdminApiError).status === 404;

  const quota = quotaQ.data ?? null;
  const rows: UsageRow[] = usageQ.data?.rows ?? [];
  const totals = usageQ.data?.totals ?? { byte_seconds: 0, objects: 0 };

  // Per-day chart aggregates avgBytes across every (bucket, class) row that
  // landed on the day. The result line is the user-wide daily average bytes.
  const chartData = useMemo(() => {
    const m = new Map<string, number>();
    for (const r of rows) {
      const v = (r.byte_seconds ?? 0) / 86400;
      m.set(r.day, (m.get(r.day) ?? 0) + v);
    }
    return Array.from(m.entries())
      .map(([day, avgBytes]) => ({ day, avgBytes }))
      .sort((a, b) => a.day.localeCompare(b.day));
  }, [rows]);

  // Per-bucket breakdown — sum byte_seconds per bucket across the window so
  // billing can rank buckets by cost contribution.
  const bucketBreakdown = useMemo(() => {
    const m = new Map<
      string,
      { bucket: string; byteSeconds: number; objectMax: number; days: number }
    >();
    for (const r of rows) {
      const b = r.bucket ?? '—';
      const cur = m.get(b) ?? {
        bucket: b,
        byteSeconds: 0,
        objectMax: 0,
        days: 0,
      };
      cur.byteSeconds += r.byte_seconds ?? 0;
      cur.objectMax = Math.max(cur.objectMax, r.object_count_max ?? 0);
      cur.days += 1;
      m.set(b, cur);
    }
    return Array.from(m.values()).sort(
      (a, b) => b.byteSeconds - a.byteSeconds,
    );
  }, [rows]);

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: queryKeys.iam.userQuota(userName) });
    void queryClient.invalidateQueries({
      queryKey: ['iam', 'user', userName, 'usage'],
    });
  }

  async function handleDeleteQuota() {
    if (!window.confirm(`Remove user-quota caps from ${userName}?`)) return;
    try {
      await deleteUserQuota(userName);
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.userQuota(userName) });
      showToast({ title: 'User quota removed', description: userName });
    } catch (err) {
      const e = err as Error;
      showToast({
        title: 'Failed to remove quota',
        description: e.message,
        variant: 'destructive',
      });
    }
  }

  if (!userName) {
    return <div className="text-sm text-muted-foreground">Missing userName.</div>;
  }

  return (
    <div className="space-y-6">
      <EditUserQuotaDialog
        open={editOpen}
        onOpenChange={setEditOpen}
        userName={userName}
        current={quota}
      />

      <div className="space-y-1">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => navigate(`/iam/users/${encodeURIComponent(userName)}`)}
          className="-ml-2 h-7 px-2 text-muted-foreground"
        >
          <ArrowLeft className="mr-1 h-3.5 w-3.5" aria-hidden />
          Back to {userName}
        </Button>
        <h1 className="text-2xl font-semibold tracking-tight">
          Billing — {userName}
        </h1>
      </div>

      {userMissing && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">User not found</div>
              <div className="text-xs text-destructive/80">
                <Link to="/iam" className="underline">
                  Return to the IAM users list
                </Link>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {!userMissing && (
        <>
          <Card>
            <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="space-y-1">
                <CardTitle className="text-base">User quota</CardTitle>
                <CardDescription>
                  Hard caps applied across every bucket{' '}
                  <code>{userName}</code> owns.
                </CardDescription>
              </div>
              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={handleRefresh}
                  disabled={usageQ.isFetching || quotaQ.isFetching}
                  aria-label="Refresh billing"
                >
                  <RefreshCw
                    className={cn(
                      'mr-1.5 h-3.5 w-3.5',
                      (usageQ.isFetching || quotaQ.isFetching) && 'animate-spin',
                    )}
                    aria-hidden
                  />
                  Refresh
                </Button>
                {quota && (
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="text-destructive hover:text-destructive"
                    onClick={handleDeleteQuota}
                    aria-label="Remove user quota"
                  >
                    <Trash2 className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                    Remove quota
                  </Button>
                )}
                <Button type="button" size="sm" onClick={() => setEditOpen(true)}>
                  <Pencil className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                  Edit user quota
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              {quotaQ.isPending ? (
                <Skeleton className="h-12 w-full" />
              ) : quota ? (
                <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
                  <div>
                    <div className="text-xs uppercase tracking-wide text-muted-foreground">
                      Max buckets
                    </div>
                    <div className="mt-1 text-base font-medium tabular-nums">
                      {quota.max_buckets > 0 ? formatCount(quota.max_buckets) : '—'}
                    </div>
                  </div>
                  <div>
                    <div className="text-xs uppercase tracking-wide text-muted-foreground">
                      Total max bytes
                    </div>
                    <div className="mt-1 text-base font-medium tabular-nums">
                      {quota.total_max_bytes > 0
                        ? formatBytes(quota.total_max_bytes)
                        : '—'}
                    </div>
                  </div>
                </div>
              ) : (
                <div className="rounded-md border border-dashed p-4 text-sm text-muted-foreground">
                  No quota set. Click{' '}
                  <span className="font-medium">Edit user quota</span> to add
                  caps.
                </div>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">30-day usage</CardTitle>
              <CardDescription>
                Daily average bytes across every bucket{' '}
                <code>{userName}</code> owns. Source:{' '}
                <code>usage_aggregates</code> rollup. Window totals:{' '}
                <span className="font-mono">
                  {formatBytes(totals.byte_seconds / 86400 / 30)}
                </span>{' '}
                avg/day · <span className="font-mono">{formatCount(totals.objects)}</span>{' '}
                peak objects.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {usageQ.isPending && !usageQ.data ? (
                <Skeleton className="h-72 w-full" />
              ) : chartData.length === 0 ? (
                <div className="py-10 text-center text-sm text-muted-foreground">
                  No usage rolled up yet for this window.
                </div>
              ) : (
                <div
                  className="h-72 w-full"
                  data-testid="user-usage-chart"
                  aria-label="30-day average bytes"
                >
                  <ResponsiveContainer width="100%" height="100%">
                    <LineChart
                      data={chartData}
                      margin={{ top: 10, right: 16, bottom: 0, left: 0 }}
                    >
                      <CartesianGrid
                        strokeDasharray="3 3"
                        stroke="hsl(var(--border))"
                      />
                      <XAxis
                        dataKey="day"
                        fontSize={11}
                        tickLine={false}
                        axisLine={false}
                      />
                      <YAxis
                        fontSize={11}
                        tickLine={false}
                        axisLine={false}
                        tickFormatter={(v: number) => formatBytes(v)}
                        width={70}
                      />
                      <Tooltip
                        formatter={(v: number) => [formatBytes(v), 'avg bytes']}
                        labelFormatter={(l) => l}
                      />
                      <Line
                        type="monotone"
                        dataKey="avgBytes"
                        stroke="hsl(220 90% 56%)"
                        strokeWidth={2}
                        dot={false}
                        isAnimationActive={false}
                      />
                    </LineChart>
                  </ResponsiveContainer>
                </div>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">Per-bucket usage</CardTitle>
              <CardDescription>
                Sorted by byte-seconds across the 30-day window — top contributors
                rank highest.
              </CardDescription>
            </CardHeader>
            <CardContent className="px-0 sm:px-0">
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="pl-4 sm:pl-6">Bucket</TableHead>
                      <TableHead className="text-right">Avg bytes / day</TableHead>
                      <TableHead className="text-right">Peak objects</TableHead>
                      <TableHead className="pr-4 text-right sm:pr-6">Days observed</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {usageQ.isPending && !usageQ.data ? (
                      Array.from({ length: 2 }).map((_, i) => (
                        <TableRow key={`sk-${i}`}>
                          <TableCell colSpan={4} className="py-3">
                            <Skeleton className="h-5 w-full" />
                          </TableCell>
                        </TableRow>
                      ))
                    ) : bucketBreakdown.length === 0 ? (
                      <TableRow>
                        <TableCell colSpan={4} className="py-6 text-center text-muted-foreground">
                          No bucket usage recorded.
                        </TableCell>
                      </TableRow>
                    ) : (
                      bucketBreakdown.map((b) => (
                        <TableRow key={b.bucket}>
                          <TableCell className="pl-4 font-medium sm:pl-6">
                            <Link
                              to={`/buckets/${encodeURIComponent(b.bucket)}`}
                              className="text-primary hover:underline"
                            >
                              {b.bucket}
                            </Link>
                          </TableCell>
                          <TableCell className="text-right tabular-nums">
                            {formatBytes(b.byteSeconds / 86400 / Math.max(1, b.days))}
                          </TableCell>
                          <TableCell className="text-right tabular-nums">
                            {formatCount(b.objectMax)}
                          </TableCell>
                          <TableCell className="pr-4 text-right tabular-nums sm:pr-6">
                            {b.days}
                          </TableCell>
                        </TableRow>
                      ))
                    )}
                  </TableBody>
                </Table>
              </div>
            </CardContent>
          </Card>
        </>
      )}
    </div>
  );
}
