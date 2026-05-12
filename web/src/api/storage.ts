// Storage backend health + per-class breakdown wrappers (US-001..US-003 of
// the web-ui-storage-status cycle). The Go-side report shapes are defined in
// internal/meta/store.go (MetaHealthReport / NodeStatus) and
// internal/data/backend.go (DataHealthReport / PoolStatus). Endpoints land in
// internal/adminapi/storage.go.

export interface NodeStatus {
  address: string;
  state: string;
  schema_version?: string;
  data_center?: string;
  rack?: string;
}

export interface MetaHealthReport {
  backend: string;
  nodes: NodeStatus[];
  replication_factor: number;
  warnings?: string[];
}

export interface PoolStatus {
  name: string;
  class: string;
  cluster?: string;
  bytes_used: number;
  object_count: number;
  num_replicas: number;
  state: string;
}

export interface DataHealthReport {
  backend: string;
  pools: PoolStatus[];
  warnings?: string[];
}

export interface StorageClassEntry {
  class: string;
  bytes: number;
  objects: number;
}

export interface StorageClassesResponse {
  classes: StorageClassEntry[];
  pools_by_class: Record<string, string>;
}

export interface StorageHealthResponse {
  ok: boolean;
  warnings: string[];
  source?: 'meta' | 'data' | string;
}

async function fetchJSON<T>(url: string, label: string): Promise<T> {
  const resp = await fetch(url, { method: 'GET', credentials: 'same-origin' });
  if (!resp.ok) throw new Error(`${label}: ${resp.status} ${resp.statusText}`);
  return (await resp.json()) as T;
}

export async function fetchStorageMeta(): Promise<MetaHealthReport> {
  const body = await fetchJSON<MetaHealthReport>(
    '/admin/v1/storage/meta',
    'storage/meta',
  );
  return {
    backend: body.backend ?? '',
    nodes: body.nodes ?? [],
    replication_factor: body.replication_factor ?? 0,
    warnings: body.warnings ?? [],
  };
}

export async function fetchStorageData(): Promise<DataHealthReport> {
  const body = await fetchJSON<DataHealthReport>(
    '/admin/v1/storage/data',
    'storage/data',
  );
  return {
    backend: body.backend ?? '',
    pools: body.pools ?? [],
    warnings: body.warnings ?? [],
  };
}

export async function fetchStorageClasses(): Promise<StorageClassesResponse> {
  const body = await fetchJSON<StorageClassesResponse>(
    '/admin/v1/storage/classes',
    'storage/classes',
  );
  return {
    classes: body.classes ?? [],
    pools_by_class: body.pools_by_class ?? {},
  };
}

export async function fetchStorageHealth(): Promise<StorageHealthResponse> {
  const body = await fetchJSON<StorageHealthResponse>(
    '/admin/v1/storage/health',
    'storage/health',
  );
  return {
    ok: body.ok ?? true,
    warnings: body.warnings ?? [],
    source: body.source,
  };
}
