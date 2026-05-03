import { QueryCache, QueryClient } from '@tanstack/react-query';

import { showToast } from './toast-store';

// QueryMeta convention — pages set { label: 'cluster status', silent?: true }
// in useQuery({ meta }) so the global error toast can show a friendly title.
export interface StrataQueryMeta extends Record<string, unknown> {
  label?: string;
  silent?: boolean;
}

declare module '@tanstack/react-query' {
  interface Register {
    queryMeta: StrataQueryMeta;
  }
}

const TOAST_THROTTLE_MS = 30_000;
const lastToastAt = new Map<string, number>();

function queryKeyToId(key: readonly unknown[]): string {
  try {
    return JSON.stringify(key);
  } catch {
    return String(key);
  }
}

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 5_000,
      refetchInterval: 5_000,
      refetchOnWindowFocus: true,
      retry: 1,
    },
  },
  queryCache: new QueryCache({
    onError: (error, query) => {
      const meta = (query.meta ?? {}) as StrataQueryMeta;
      if (meta.silent) return;
      const id = queryKeyToId(query.queryKey);
      const now = Date.now();
      const prev = lastToastAt.get(id) ?? 0;
      if (now - prev < TOAST_THROTTLE_MS) return;
      lastToastAt.set(id, now);
      const label = meta.label ?? 'data';
      const message = error instanceof Error ? error.message : String(error);
      showToast({
        title: `Failed to load ${label}`,
        description: message,
        variant: 'destructive',
        action: {
          label: 'Retry',
          onClick: () => {
            void queryClient.invalidateQueries({ queryKey: query.queryKey });
          },
        },
      });
    },
  }),
});

export const queryKeys = {
  cluster: {
    status: ['cluster', 'status'] as const,
    nodes: ['cluster', 'nodes'] as const,
  },
  buckets: {
    top: (by: string, limit: number) => ['buckets', 'top', by, limit] as const,
    list: (query: string, sort: string, order: string, page: number, pageSize: number) =>
      ['buckets', 'list', { query, sort, order, page, pageSize }] as const,
    one: (name: string) => ['buckets', 'detail', name] as const,
    objects: (name: string, prefix: string, marker: string) =>
      ['buckets', 'objects', name, { prefix, marker }] as const,
    objectLock: (name: string) => ['buckets', 'object-lock', name] as const,
    lifecycle: (name: string) => ['buckets', 'lifecycle', name] as const,
  },
  consumers: {
    top: (by: string, limit: number) => ['consumers', 'top', by, limit] as const,
  },
  metrics: {
    timeseries: (metric: string, range: string, step: string) =>
      ['metrics', 'timeseries', metric, range, step] as const,
  },
  auth: {
    whoami: ['auth', 'whoami'] as const,
  },
};
