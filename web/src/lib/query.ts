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
    cors: (name: string) => ['buckets', 'cors', name] as const,
    policy: (name: string) => ['buckets', 'policy', name] as const,
    acl: (name: string) => ['buckets', 'acl', name] as const,
    inventory: (name: string) => ['buckets', 'inventory', name] as const,
    logging: (name: string) => ['buckets', 'logging', name] as const,
    object: (name: string, key: string, versionID: string) =>
      ['buckets', 'object', name, key, versionID] as const,
    objectVersions: (name: string, key: string) =>
      ['buckets', 'object-versions', name, key] as const,
    distribution: (name: string) => ['buckets', 'distribution', name] as const,
    replicationLag: (name: string, range: string) =>
      ['buckets', 'replication-lag', name, range] as const,
    quota: (name: string) => ['buckets', 'quota', name] as const,
    placement: (name: string) => ['buckets', 'placement', name] as const,
    usage: (name: string, start: string, end: string) =>
      ['buckets', 'usage', name, { start, end }] as const,
  },
  consumers: {
    top: (by: string, limit: number) => ['consumers', 'top', by, limit] as const,
  },
  iam: {
    users: (query: string, page: number, pageSize: number) =>
      ['iam', 'users', { query, page, pageSize }] as const,
    user: (userName: string) => ['iam', 'user', userName] as const,
    accessKeys: (userName: string) =>
      ['iam', 'user', userName, 'access-keys'] as const,
    userPolicies: (userName: string) =>
      ['iam', 'user', userName, 'policies'] as const,
    policies: ['iam', 'policies'] as const,
    userQuota: (userName: string) =>
      ['iam', 'user', userName, 'quota'] as const,
    userUsage: (userName: string, start: string, end: string) =>
      ['iam', 'user', userName, 'usage', { start, end }] as const,
  },
  metrics: {
    timeseries: (metric: string, range: string, step: string) =>
      ['metrics', 'timeseries', metric, range, step] as const,
  },
  multipart: {
    active: (
      bucket: string,
      minAgeHours: number,
      initiator: string,
      page: number,
      pageSize: number,
    ) =>
      [
        'multipart',
        'active',
        { bucket, minAgeHours, initiator, page, pageSize },
      ] as const,
  },
  audit: {
    list: (
      since: string,
      until: string,
      action: string,
      principal: string,
      bucket: string,
      pageToken: string,
    ) =>
      [
        'audit',
        'list',
        { since, until, action, principal, bucket, pageToken },
      ] as const,
  },
  diagnostics: {
    slowQueries: (since: string, minMs: number, pageToken: string) =>
      ['diagnostics', 'slow-queries', { since, minMs, pageToken }] as const,
    trace: (idOrRequestID: string) =>
      ['diagnostics', 'trace', idOrRequestID] as const,
    hotBuckets: (range: string, step: string) =>
      ['diagnostics', 'hot-buckets', { range, step }] as const,
    hotShards: (bucket: string, range: string, step: string) =>
      ['diagnostics', 'hot-shards', bucket, { range, step }] as const,
    node: (nodeID: string, range: string) =>
      ['diagnostics', 'node', nodeID, range] as const,
  },
  auth: {
    whoami: ['auth', 'whoami'] as const,
  },
  settings: {
    all: ['settings', 'all'] as const,
    dataBackend: ['settings', 'data-backend'] as const,
  },
  storage: {
    meta: ['storage', 'meta'] as const,
    data: ['storage', 'data'] as const,
    classes: ['storage', 'classes'] as const,
    health: ['storage', 'health'] as const,
  },
  clusters: ['clusters'] as const,
  clusterRebalance: (id: string) => ['clusters', 'rebalance', id] as const,
};
