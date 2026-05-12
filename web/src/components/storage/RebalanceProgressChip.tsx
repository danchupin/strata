import { useMemo } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Line, LineChart, ResponsiveContainer } from 'recharts';

import { fetchClusterRebalanceProgress } from '@/api/client';
import { queryKeys } from '@/lib/query';

const REBALANCE_POLL_MS = 30_000;

interface Props {
  clusterID: string;
}

function formatCount(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return n.toFixed(0);
}

// RebalanceProgressChip surfaces the per-cluster rebalance counters +
// 1h sparkline below the size/state row of a cluster card (US-005
// placement-ui). Shared query key per cluster id dedups across mounts.
// Backend degrades to metrics_available=false when Prometheus is unset
// or unreachable; the chip renders "(metrics unavailable)" instead of
// erroring so the card still mounts.
export function RebalanceProgressChip({ clusterID }: Props) {
  const q = useQuery({
    queryKey: queryKeys.clusterRebalance(clusterID),
    queryFn: () => fetchClusterRebalanceProgress(clusterID),
    refetchInterval: REBALANCE_POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: `rebalance progress ${clusterID}`, silent: true },
  });

  const points = useMemo(
    () => (q.data?.series ?? []).map(([ts, value]) => ({ ts, value })),
    [q.data?.series],
  );

  if (q.isPending && !q.data) {
    return (
      <div className="text-xs text-muted-foreground">Loading rebalance…</div>
    );
  }
  if (!q.data?.metrics_available) {
    return (
      <div className="text-xs text-muted-foreground">(metrics unavailable)</div>
    );
  }

  return (
    <div className="flex items-center gap-3">
      <div className="tabular-nums text-xs text-muted-foreground">
        <span className="font-medium text-foreground">
          {formatCount(q.data.moved_total)}
        </span>{' '}
        chunks moved · {formatCount(q.data.refused_total)} refused
      </div>
      <div className="h-6 flex-1 min-w-[64px]" aria-label="rebalance rate sparkline">
        {points.length > 1 ? (
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={points} margin={{ top: 2, right: 2, left: 2, bottom: 2 }}>
              <Line
                type="monotone"
                dataKey="value"
                stroke="hsl(220 90% 56%)"
                strokeWidth={1.5}
                dot={false}
                isAnimationActive={false}
              />
            </LineChart>
          </ResponsiveContainer>
        ) : null}
      </div>
    </div>
  );
}
