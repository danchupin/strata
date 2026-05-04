import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { AlertTriangle, Plus, Trash2, X } from 'lucide-react';

import {
  deleteObject,
  fetchObjectDetail,
  fetchObjectVersions,
  setObjectLegalHold,
  setObjectRetention,
  setObjectTags,
  type AdminApiError,
  type BucketDetail,
  type ObjectDetail,
  type ObjectRetentionMode,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
import { Skeleton } from '@/components/ui/skeleton';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';

interface Props {
  bucket: string;
  bucketDetail: BucketDetail | undefined;
  objectKey: string | null;
  onClose: () => void;
  onObjectDeleted?: () => void;
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  let v = bytes;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function formatDate(epochSec: number | undefined | null): string {
  if (!epochSec) return '—';
  return new Date(epochSec * 1000).toLocaleString();
}

// toLocalInput renders an epoch-second timestamp into a value the
// <input type="datetime-local"> control accepts. Local timezone, no seconds.
function toLocalInput(epochSec: number): string {
  const d = new Date(epochSec * 1000);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(
    d.getHours(),
  )}:${pad(d.getMinutes())}`;
}

export function ObjectDetailSheet({
  bucket,
  bucketDetail,
  objectKey,
  onClose,
  onObjectDeleted,
}: Props) {
  const open = Boolean(objectKey);
  return (
    <Sheet open={open} onOpenChange={(v) => !v && onClose()}>
      <SheetContent
        side="right"
        className="w-full overflow-y-auto sm:max-w-xl"
      >
        {objectKey && (
          <ObjectDetailBody
            bucket={bucket}
            bucketDetail={bucketDetail}
            objectKey={objectKey}
            onClose={onClose}
            onObjectDeleted={onObjectDeleted}
          />
        )}
      </SheetContent>
    </Sheet>
  );
}

function ObjectDetailBody({
  bucket,
  bucketDetail,
  objectKey,
  onClose,
  onObjectDeleted,
}: {
  bucket: string;
  bucketDetail: BucketDetail | undefined;
  objectKey: string;
  onClose: () => void;
  onObjectDeleted?: () => void;
}) {
  const detailQ = useQuery<ObjectDetail>({
    queryKey: queryKeys.buckets.object(bucket, objectKey, ''),
    queryFn: () => fetchObjectDetail(bucket, objectKey),
    enabled: Boolean(objectKey),
    refetchInterval: false,
    meta: { label: `object ${objectKey}` },
  });
  const detail = detailQ.data;
  const lockEnabled = Boolean(bucketDetail?.object_lock);

  return (
    <>
      <SheetHeader className="pr-8">
        <SheetTitle className="break-all text-base">{objectKey}</SheetTitle>
        <SheetDescription className="break-all">
          <code className="text-xs">
            {bucket}/{objectKey}
          </code>
        </SheetDescription>
      </SheetHeader>

      <Tabs defaultValue="overview" className="mt-4">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="tags">Tags</TabsTrigger>
          <TabsTrigger value="retention">Retention</TabsTrigger>
          <TabsTrigger value="legal-hold">Legal Hold</TabsTrigger>
          <TabsTrigger value="versions">Versions</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="mt-4 space-y-4">
          <OverviewTab detail={detail} loading={detailQ.isPending} />
          <div className="flex justify-end">
            <DeleteObjectButton
              bucket={bucket}
              objectKey={objectKey}
              onDeleted={() => {
                onObjectDeleted?.();
                onClose();
              }}
            />
          </div>
        </TabsContent>

        <TabsContent value="tags" className="mt-4 space-y-4">
          <TagsTab
            bucket={bucket}
            objectKey={objectKey}
            initialTags={detail?.tags ?? {}}
            loading={detailQ.isPending}
          />
        </TabsContent>

        <TabsContent value="retention" className="mt-4 space-y-4">
          <RetentionTab
            bucket={bucket}
            objectKey={objectKey}
            detail={detail}
            loading={detailQ.isPending}
            lockEnabled={lockEnabled}
          />
        </TabsContent>

        <TabsContent value="legal-hold" className="mt-4 space-y-4">
          <LegalHoldTab
            bucket={bucket}
            objectKey={objectKey}
            detail={detail}
            loading={detailQ.isPending}
            lockEnabled={lockEnabled}
          />
        </TabsContent>

        <TabsContent value="versions" className="mt-4 space-y-4">
          <VersionsTab
            bucket={bucket}
            objectKey={objectKey}
            onAnyVersionDeleted={() => {
              void queryClient.invalidateQueries({
                queryKey: queryKeys.buckets.object(bucket, objectKey, ''),
              });
              void queryClient.invalidateQueries({
                queryKey: ['buckets', 'objects', bucket],
              });
            }}
          />
        </TabsContent>
      </Tabs>
    </>
  );
}

function OverviewTab({
  detail,
  loading,
}: {
  detail: ObjectDetail | undefined;
  loading: boolean;
}) {
  if (loading || !detail) {
    return (
      <div className="space-y-2">
        {Array.from({ length: 5 }).map((_, i) => (
          <Skeleton key={i} className="h-5 w-full" />
        ))}
      </div>
    );
  }
  const rows: Array<{ label: string; value: React.ReactNode }> = [
    { label: 'Size', value: formatBytes(detail.size) },
    { label: 'Last modified', value: formatDate(detail.last_modified) },
    { label: 'Storage class', value: detail.storage_class || '—' },
    { label: 'Content type', value: detail.content_type || '—' },
    {
      label: 'ETag',
      value: <code className="break-all font-mono text-xs">{detail.etag || '—'}</code>,
    },
    {
      label: 'Version',
      value: detail.version_id ? (
        <code className="break-all font-mono text-xs">{detail.version_id}</code>
      ) : (
        '—'
      ),
    },
    {
      label: 'Status',
      value: (
        <div className="flex flex-wrap gap-1">
          {detail.is_latest && <Badge variant="success">Latest</Badge>}
          {detail.is_delete_marker && (
            <Badge variant="warning">Delete marker</Badge>
          )}
          {!detail.is_latest && !detail.is_delete_marker && (
            <Badge variant="secondary">Noncurrent</Badge>
          )}
        </div>
      ),
    },
  ];
  return (
    <dl className="grid grid-cols-1 gap-3 text-sm">
      {rows.map((r) => (
        <div
          key={r.label}
          className="grid grid-cols-[120px_1fr] items-baseline gap-2"
        >
          <dt className="text-xs uppercase tracking-wide text-muted-foreground">
            {r.label}
          </dt>
          <dd>{r.value}</dd>
        </div>
      ))}
    </dl>
  );
}

function TagsTab({
  bucket,
  objectKey,
  initialTags,
  loading,
}: {
  bucket: string;
  objectKey: string;
  initialTags: Record<string, string>;
  loading: boolean;
}) {
  // Local edit state — array form so duplicate-key drafts don't merge.
  const [rows, setRows] = useState<Array<{ key: string; value: string }>>(() =>
    Object.entries(initialTags).map(([key, value]) => ({ key, value })),
  );
  const [serverError, setServerError] = useState<string | null>(null);

  // When the underlying object changes (different key) reset local rows.
  useEffect(() => {
    setRows(
      Object.entries(initialTags).map(([key, value]) => ({ key, value })),
    );
    setServerError(null);
  }, [initialTags, objectKey]);

  const mutation = useMutation({
    mutationFn: async () => {
      const map: Record<string, string> = {};
      for (const r of rows) {
        const k = r.key.trim();
        if (!k) continue;
        map[k] = r.value;
      }
      await setObjectTags(bucket, objectKey, map);
    },
    onSuccess: () => {
      showToast({ title: 'Tags saved', variant: 'default' });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.object(bucket, objectKey, ''),
      });
    },
    onError: (err) => {
      const e = err as AdminApiError;
      setServerError(e.message ?? 'failed');
    },
  });

  if (loading) {
    return <Skeleton className="h-32 w-full" />;
  }

  return (
    <div className="space-y-3">
      <div className="space-y-2">
        {rows.length === 0 && (
          <p className="text-xs text-muted-foreground">
            No tags. Add one below.
          </p>
        )}
        {rows.map((r, idx) => (
          <div key={idx} className="flex items-center gap-2">
            <Input
              aria-label={`Tag key ${idx + 1}`}
              placeholder="key"
              value={r.key}
              onChange={(e) =>
                setRows((prev) =>
                  prev.map((p, i) => (i === idx ? { ...p, key: e.target.value } : p)),
                )
              }
              className="h-8 max-w-[180px]"
            />
            <Input
              aria-label={`Tag value ${idx + 1}`}
              placeholder="value"
              value={r.value}
              onChange={(e) =>
                setRows((prev) =>
                  prev.map((p, i) => (i === idx ? { ...p, value: e.target.value } : p)),
                )
              }
              className="h-8"
            />
            <Button
              type="button"
              size="icon"
              variant="ghost"
              aria-label="Remove tag"
              onClick={() => setRows((prev) => prev.filter((_, i) => i !== idx))}
            >
              <X className="h-4 w-4" aria-hidden />
            </Button>
          </div>
        ))}
      </div>
      <Button
        type="button"
        size="sm"
        variant="outline"
        onClick={() => setRows((prev) => [...prev, { key: '', value: '' }])}
      >
        <Plus className="mr-1 h-3.5 w-3.5" aria-hidden />
        Add tag
      </Button>
      {serverError && (
        <div className="text-xs text-destructive">{serverError}</div>
      )}
      <div className="flex justify-end">
        <Button
          type="button"
          size="sm"
          onClick={() => {
            setServerError(null);
            mutation.mutate();
          }}
          disabled={mutation.isPending}
        >
          {mutation.isPending ? 'Saving…' : 'Save tags'}
        </Button>
      </div>
    </div>
  );
}

function RetentionTab({
  bucket,
  objectKey,
  detail,
  loading,
  lockEnabled,
}: {
  bucket: string;
  objectKey: string;
  detail: ObjectDetail | undefined;
  loading: boolean;
  lockEnabled: boolean;
}) {
  const [mode, setMode] = useState<ObjectRetentionMode>('None');
  const [until, setUntil] = useState<string>('');
  const [serverError, setServerError] = useState<string | null>(null);

  useEffect(() => {
    if (!detail) return;
    const m = detail.retain_mode;
    if (m === 'GOVERNANCE' || m === 'COMPLIANCE') {
      setMode(m);
    } else {
      setMode('None');
    }
    if (detail.retain_until) {
      setUntil(toLocalInput(detail.retain_until));
    } else {
      setUntil('');
    }
    setServerError(null);
  }, [detail, objectKey]);

  const mutation = useMutation({
    mutationFn: async () => {
      const iso =
        mode !== 'None' && until ? new Date(until).toISOString() : null;
      await setObjectRetention(bucket, objectKey, mode, iso);
    },
    onSuccess: () => {
      showToast({ title: 'Retention saved', variant: 'default' });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.object(bucket, objectKey, ''),
      });
    },
    onError: (err) => {
      const e = err as AdminApiError;
      setServerError(e.message ?? 'failed');
    },
  });

  if (loading) return <Skeleton className="h-32 w-full" />;

  const disabled = !lockEnabled;

  return (
    <div className="space-y-3">
      {disabled && (
        <div className="rounded border border-amber-500/40 bg-amber-500/5 p-3 text-xs text-amber-600 dark:text-amber-400">
          <AlertTriangle className="mr-1 inline h-3.5 w-3.5" aria-hidden />
          Bucket has Object-Lock disabled. Enable it on the Overview tab before
          setting per-object retention.
        </div>
      )}
      <div className="space-y-2">
        <Label>Mode</Label>
        <div className="flex flex-wrap gap-2">
          {(['None', 'GOVERNANCE', 'COMPLIANCE'] as ObjectRetentionMode[]).map(
            (m) => (
              <Button
                key={m}
                type="button"
                size="sm"
                variant={mode === m ? 'default' : 'outline'}
                onClick={() => setMode(m)}
                disabled={disabled}
              >
                {m === 'None' ? 'None' : m}
              </Button>
            ),
          )}
        </div>
      </div>
      <div className="space-y-2">
        <Label htmlFor="retain-until">Retain until</Label>
        <Input
          id="retain-until"
          type="datetime-local"
          value={until}
          onChange={(e) => setUntil(e.target.value)}
          disabled={disabled || mode === 'None'}
        />
      </div>
      {serverError && (
        <div className="text-xs text-destructive">{serverError}</div>
      )}
      <div className="flex justify-end">
        <Button
          type="button"
          size="sm"
          disabled={disabled || mutation.isPending}
          onClick={() => {
            setServerError(null);
            mutation.mutate();
          }}
        >
          {mutation.isPending ? 'Saving…' : 'Save retention'}
        </Button>
      </div>
    </div>
  );
}

function LegalHoldTab({
  bucket,
  objectKey,
  detail,
  loading,
  lockEnabled,
}: {
  bucket: string;
  objectKey: string;
  detail: ObjectDetail | undefined;
  loading: boolean;
  lockEnabled: boolean;
}) {
  const [enabled, setEnabled] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);
  useEffect(() => {
    if (!detail) return;
    setEnabled(detail.legal_hold);
    setServerError(null);
  }, [detail, objectKey]);

  const mutation = useMutation({
    mutationFn: () => setObjectLegalHold(bucket, objectKey, enabled),
    onSuccess: () => {
      showToast({ title: 'Legal hold saved', variant: 'default' });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.object(bucket, objectKey, ''),
      });
    },
    onError: (err) => {
      const e = err as AdminApiError;
      setServerError(e.message ?? 'failed');
    },
  });

  if (loading) return <Skeleton className="h-24 w-full" />;

  const turnOnDisabled = !lockEnabled && !enabled;

  return (
    <div className="space-y-3">
      {!lockEnabled && (
        <div className="rounded border border-amber-500/40 bg-amber-500/5 p-3 text-xs text-amber-600 dark:text-amber-400">
          <AlertTriangle className="mr-1 inline h-3.5 w-3.5" aria-hidden />
          Bucket has Object-Lock disabled. Turning legal hold on requires it
          first.
        </div>
      )}
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(e) => setEnabled(e.target.checked)}
          disabled={turnOnDisabled}
          className="h-4 w-4"
        />
        Legal hold ON
      </label>
      {serverError && (
        <div className="text-xs text-destructive">{serverError}</div>
      )}
      <div className="flex justify-end">
        <Button
          type="button"
          size="sm"
          onClick={() => {
            setServerError(null);
            mutation.mutate();
          }}
          disabled={mutation.isPending}
        >
          {mutation.isPending ? 'Saving…' : 'Save legal hold'}
        </Button>
      </div>
    </div>
  );
}

function VersionsTab({
  bucket,
  objectKey,
  onAnyVersionDeleted,
}: {
  bucket: string;
  objectKey: string;
  onAnyVersionDeleted: () => void;
}) {
  const versionsQ = useQuery({
    queryKey: queryKeys.buckets.objectVersions(bucket, objectKey),
    queryFn: () => fetchObjectVersions(bucket, objectKey),
    enabled: Boolean(objectKey),
    refetchInterval: false,
    meta: { label: `versions ${objectKey}` },
  });

  const deleteVersion = useMutation({
    mutationFn: (versionID: string) =>
      deleteObject(bucket, objectKey, versionID),
    onSuccess: () => {
      showToast({ title: 'Version deleted', variant: 'default' });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.objectVersions(bucket, objectKey),
      });
      onAnyVersionDeleted();
    },
    onError: (err) => {
      const e = err as AdminApiError;
      showToast({
        title: 'Delete failed',
        description: e.message,
        variant: 'destructive',
      });
    },
  });

  const versions = versionsQ.data ?? [];

  const visibleVersions = useMemo(
    () =>
      versions.map((v) => ({
        ...v,
        shortVersion:
          v.version_id.length > 12
            ? `${v.version_id.slice(0, 8)}…${v.version_id.slice(-4)}`
            : v.version_id || '—',
      })),
    [versions],
  );

  if (versionsQ.isPending) {
    return (
      <div className="space-y-2">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-9 w-full" />
        ))}
      </div>
    );
  }
  if (visibleVersions.length === 0) {
    return (
      <p className="text-xs text-muted-foreground">
        No version history. Bucket may not have versioning enabled.
      </p>
    );
  }
  return (
    <div className="space-y-2">
      {visibleVersions.map((v) => (
        <div
          key={v.version_id || 'null'}
          className={
            'flex items-center justify-between gap-2 rounded border px-3 py-2 text-xs ' +
            (v.is_delete_marker ? 'border-warning/40 bg-warning/5' : '')
          }
        >
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-1">
              <code className="font-mono">{v.shortVersion}</code>
              {v.is_latest && <Badge variant="success">Latest</Badge>}
              {v.is_delete_marker && (
                <Badge variant="warning">Delete marker</Badge>
              )}
            </div>
            <div className="mt-0.5 text-muted-foreground">
              {formatBytes(v.size)} · {formatDate(v.last_modified)}
            </div>
          </div>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            className="text-destructive hover:bg-destructive/10"
            onClick={() => deleteVersion.mutate(v.version_id)}
            disabled={deleteVersion.isPending}
            aria-label="Delete this version"
          >
            <Trash2 className="h-3.5 w-3.5" aria-hidden />
          </Button>
        </div>
      ))}
    </div>
  );
}

function DeleteObjectButton({
  bucket,
  objectKey,
  onDeleted,
}: {
  bucket: string;
  objectKey: string;
  onDeleted: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const mutation = useMutation({
    mutationFn: () => deleteObject(bucket, objectKey),
    onSuccess: () => {
      showToast({ title: 'Object deleted', variant: 'default' });
      void queryClient.invalidateQueries({
        queryKey: ['buckets', 'objects', bucket],
      });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.one(bucket),
      });
      onDeleted();
    },
    onError: (err) => {
      const e = err as AdminApiError;
      showToast({
        title: 'Delete failed',
        description: e.message,
        variant: 'destructive',
      });
    },
  });
  if (!confirming) {
    return (
      <Button
        type="button"
        size="sm"
        variant="destructive"
        onClick={() => setConfirming(true)}
      >
        <Trash2 className="mr-1.5 h-3.5 w-3.5" aria-hidden />
        Delete object
      </Button>
    );
  }
  return (
    <div className="flex items-center gap-2">
      <span className="text-xs text-muted-foreground">Delete this object?</span>
      <Button
        type="button"
        size="sm"
        variant="outline"
        onClick={() => setConfirming(false)}
      >
        Cancel
      </Button>
      <Button
        type="button"
        size="sm"
        variant="destructive"
        onClick={() => mutation.mutate()}
        disabled={mutation.isPending}
      >
        {mutation.isPending ? 'Deleting…' : 'Confirm delete'}
      </Button>
    </div>
  );
}
