import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, Loader2, Pencil, Plus, Trash2 } from 'lucide-react';

import {
  deleteBucketInventory,
  fetchBucketsList,
  listBucketInventory,
  setBucketInventory,
  type AdminApiError,
  type BucketDetail,
  type InventoryConfig,
  type InventoryFormat,
  type InventoryFrequency,
  type InventoryVersions,
} from '@/api/client';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

const FORMATS: ReadonlyArray<InventoryFormat> = ['CSV', 'ORC', 'Parquet'];
const FREQUENCIES: ReadonlyArray<InventoryFrequency> = ['Daily', 'Hourly', 'Weekly'];
const VERSIONS: ReadonlyArray<InventoryVersions> = ['Current', 'All'];

// AWS-spec optional fields the s3api consumer round-trips. The worker emits a
// fixed CSV header today; the OptionalFields list is preserved on the blob
// for future consumers that filter on the configured set.
const OPTIONAL_FIELDS: ReadonlyArray<string> = [
  'Size',
  'LastModifiedDate',
  'StorageClass',
  'ETag',
  'IsMultipartUploaded',
  'ReplicationStatus',
  'EncryptionStatus',
  'ObjectLockRetainUntilDate',
  'ObjectLockMode',
  'ObjectLockLegalHoldStatus',
  'IntelligentTieringAccessTier',
  'BucketKeyStatus',
  'ChecksumAlgorithm',
  'ObjectAccessControlList',
  'ObjectOwner',
];

interface Props {
  bucket: BucketDetail;
}

function newConfig(): InventoryConfig {
  return {
    id: '',
    is_enabled: true,
    destination: { bucket: '', format: 'CSV', prefix: '' },
    schedule: { frequency: 'Daily' },
    included_object_versions: 'Current',
    filter: undefined,
    optional_fields: [],
  };
}

export function BucketInventoryTab({ bucket }: Props) {
  const listQ = useQuery({
    queryKey: queryKeys.buckets.inventory(bucket.name),
    queryFn: () => listBucketInventory(bucket.name),
    meta: { silent: true },
  });

  const [editor, setEditor] = useState<{
    open: boolean;
    cfg: InventoryConfig;
    isNew: boolean;
  }>({ open: false, cfg: newConfig(), isNew: true });

  const configs = listQ.data?.configurations ?? [];

  function openCreate() {
    setEditor({ open: true, cfg: newConfig(), isNew: true });
  }

  function openEdit(cfg: InventoryConfig) {
    setEditor({ open: true, cfg: { ...cfg }, isNew: false });
  }

  async function handleDelete(id: string) {
    if (!window.confirm(`Delete inventory configuration "${id}"?`)) return;
    try {
      await deleteBucketInventory(bucket.name, id);
      await queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.inventory(bucket.name),
      });
      showToast({ title: 'Inventory configuration deleted', description: id });
    } catch (err) {
      const e = err as AdminApiError;
      showToast({
        title: 'Delete failed',
        description: `${e.code ?? 'Error'}: ${e.message}`,
        variant: 'destructive',
      });
    }
  }

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Inventory configurations</CardTitle>
            <CardDescription>
              Schedule periodic CSV/ORC/Parquet inventories of this bucket. Worker
              writes manifest.json + data files to the destination bucket on the
              configured cadence.
            </CardDescription>
          </div>
          <Button type="button" size="sm" onClick={openCreate}>
            <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden />
            Add configuration
          </Button>
        </CardHeader>
        <CardContent className="px-0">
          {listQ.isPending && (
            <div className="px-4 py-6 text-sm text-muted-foreground">Loading…</div>
          )}
          {!listQ.isPending && configs.length === 0 && (
            <div className="px-4 py-6 text-sm text-muted-foreground">
              No inventory configurations yet. Click <strong>Add configuration</strong>{' '}
              to create one.
            </div>
          )}
          {configs.length > 0 && (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="pl-4 sm:pl-6">ID</TableHead>
                    <TableHead>Schedule</TableHead>
                    <TableHead>Destination</TableHead>
                    <TableHead>Format</TableHead>
                    <TableHead>Versions</TableHead>
                    <TableHead>Enabled</TableHead>
                    <TableHead className="pr-4 text-right sm:pr-6">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {configs.map((c) => (
                    <TableRow key={c.id}>
                      <TableCell className="pl-4 font-medium sm:pl-6">{c.id}</TableCell>
                      <TableCell>{c.schedule.frequency}</TableCell>
                      <TableCell className="font-mono text-xs">
                        {c.destination.bucket}
                        {c.destination.prefix ? `/${c.destination.prefix}` : ''}
                      </TableCell>
                      <TableCell>{c.destination.format}</TableCell>
                      <TableCell>{c.included_object_versions}</TableCell>
                      <TableCell>{c.is_enabled ? 'Yes' : 'No'}</TableCell>
                      <TableCell className="pr-4 text-right sm:pr-6">
                        <div className="flex justify-end gap-1">
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={() => openEdit(c)}
                          >
                            <Pencil className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                            Edit
                          </Button>
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={() => void handleDelete(c.id)}
                          >
                            <Trash2 className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                            Delete
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      <InventoryEditorDialog
        bucketName={bucket.name}
        open={editor.open}
        isNew={editor.isNew}
        initial={editor.cfg}
        existingIDs={configs.map((c) => c.id)}
        onOpenChange={(open) => setEditor((prev) => ({ ...prev, open }))}
      />
    </div>
  );
}

function InventoryEditorDialog({
  bucketName,
  open,
  isNew,
  initial,
  existingIDs,
  onOpenChange,
}: {
  bucketName: string;
  open: boolean;
  isNew: boolean;
  initial: InventoryConfig;
  existingIDs: string[];
  onOpenChange: (open: boolean) => void;
}) {
  const [cfg, setCfg] = useState<InventoryConfig>(initial);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (open) {
      setCfg(initial);
      setServerError(null);
    }
  }, [open, initial]);

  // Destination-bucket autocomplete: fetch first page of buckets and filter
  // client-side. Page size 1000 is enough for typical clusters; for clusters
  // with > 1000 buckets the operator can fall back to typing the full name.
  const bucketsQ = useQuery({
    queryKey: ['buckets', 'list', { query: '', sort: '', order: 'asc', page: 1, pageSize: 1000 }],
    queryFn: () => fetchBucketsList({ pageSize: 1000 }),
    meta: { silent: true },
    enabled: open,
  });
  const bucketSuggestions = useMemo(
    () => (bucketsQ.data?.buckets ?? []).map((b) => b.name).sort(),
    [bucketsQ.data],
  );

  function patch(p: Partial<InventoryConfig>) {
    setCfg((prev) => ({ ...prev, ...p }));
  }

  function patchDest(p: Partial<InventoryConfig['destination']>) {
    setCfg((prev) => ({ ...prev, destination: { ...prev.destination, ...p } }));
  }

  function toggleField(field: string) {
    setCfg((prev) => {
      const cur = prev.optional_fields ?? [];
      if (cur.includes(field)) {
        return { ...prev, optional_fields: cur.filter((f) => f !== field) };
      }
      return { ...prev, optional_fields: [...cur, field] };
    });
  }

  async function handleSubmit() {
    setServerError(null);
    const id = cfg.id.trim();
    if (!id) {
      setServerError({ code: 'InvalidArgument', message: 'ID is required' });
      return;
    }
    if (isNew && existingIDs.includes(id)) {
      setServerError({
        code: 'Conflict',
        message: `A configuration with ID "${id}" already exists`,
      });
      return;
    }
    if (!cfg.destination.bucket.trim()) {
      setServerError({ code: 'InvalidArgument', message: 'Destination bucket is required' });
      return;
    }
    setSaving(true);
    try {
      const payload: InventoryConfig = {
        ...cfg,
        id,
        filter:
          cfg.filter && cfg.filter.prefix && cfg.filter.prefix.trim() !== ''
            ? { prefix: cfg.filter.prefix }
            : undefined,
        optional_fields:
          cfg.optional_fields && cfg.optional_fields.length > 0
            ? cfg.optional_fields
            : undefined,
      };
      await setBucketInventory(bucketName, id, payload);
      await queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.inventory(bucketName),
      });
      showToast({
        title: isNew ? 'Inventory configuration created' : 'Inventory configuration saved',
        description: id,
      });
      onOpenChange(false);
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !saving && onOpenChange(o)}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>
            {isNew ? 'Add inventory configuration' : `Edit "${initial.id}"`}
          </DialogTitle>
          <DialogDescription>
            Strata writes manifest.json + data files into the destination bucket on the
            schedule below.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          <div className="grid gap-2 sm:grid-cols-2">
            <div>
              <Label className="text-xs" htmlFor="inv-id">
                ID
              </Label>
              <Input
                id="inv-id"
                value={cfg.id}
                onChange={(e) => patch({ id: e.target.value })}
                disabled={!isNew}
                placeholder="daily-csv"
              />
              {!isNew && (
                <p className="mt-1 text-xs text-muted-foreground">
                  Configuration ID cannot be changed.
                </p>
              )}
            </div>
            <div>
              <Label className="text-xs">Status</Label>
              <label className="flex h-10 items-center gap-2 rounded-md border px-3 text-sm">
                <input
                  type="checkbox"
                  checked={cfg.is_enabled}
                  onChange={(e) => patch({ is_enabled: e.target.checked })}
                />
                <span>Enabled</span>
              </label>
            </div>
          </div>

          <div className="grid gap-2 sm:grid-cols-3">
            <div>
              <Label className="text-xs">Schedule</Label>
              <Select
                value={cfg.schedule.frequency}
                onValueChange={(v) =>
                  patch({ schedule: { frequency: v as InventoryFrequency } })
                }
              >
                <SelectTrigger className="h-9">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {FREQUENCIES.map((f) => (
                    <SelectItem key={f} value={f}>
                      {f}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div>
              <Label className="text-xs">Object versions</Label>
              <Select
                value={cfg.included_object_versions}
                onValueChange={(v) =>
                  patch({ included_object_versions: v as InventoryVersions })
                }
              >
                <SelectTrigger className="h-9">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {VERSIONS.map((v) => (
                    <SelectItem key={v} value={v}>
                      {v}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div>
              <Label className="text-xs">Format</Label>
              <Select
                value={cfg.destination.format}
                onValueChange={(v) => patchDest({ format: v as InventoryFormat })}
              >
                <SelectTrigger className="h-9">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {FORMATS.map((f) => (
                    <SelectItem key={f} value={f}>
                      {f}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-2 sm:grid-cols-2">
            <div>
              <Label className="text-xs" htmlFor="inv-dest-bucket">
                Destination bucket
              </Label>
              <Input
                id="inv-dest-bucket"
                value={cfg.destination.bucket}
                onChange={(e) => patchDest({ bucket: e.target.value })}
                placeholder="logs-bucket"
                list="inv-dest-buckets"
              />
              <datalist id="inv-dest-buckets">
                {bucketSuggestions.map((b) => (
                  <option key={b} value={b} />
                ))}
              </datalist>
            </div>
            <div>
              <Label className="text-xs" htmlFor="inv-dest-prefix">
                Destination prefix (optional)
              </Label>
              <Input
                id="inv-dest-prefix"
                value={cfg.destination.prefix ?? ''}
                onChange={(e) => patchDest({ prefix: e.target.value })}
                placeholder="inventory/"
              />
            </div>
          </div>

          <div>
            <Label className="text-xs" htmlFor="inv-filter-prefix">
              Filter prefix (optional)
            </Label>
            <Input
              id="inv-filter-prefix"
              value={cfg.filter?.prefix ?? ''}
              onChange={(e) =>
                patch({
                  filter: e.target.value ? { prefix: e.target.value } : undefined,
                })
              }
              placeholder="logs/"
            />
            <p className="mt-1 text-xs text-muted-foreground">
              Limits the inventory to objects whose key starts with this prefix.
            </p>
          </div>

          <div>
            <Label className="text-xs">Optional fields</Label>
            <div className="grid gap-1.5 rounded-md border p-2 sm:grid-cols-2">
              {OPTIONAL_FIELDS.map((f) => {
                const checked = (cfg.optional_fields ?? []).includes(f);
                return (
                  <label
                    key={f}
                    className="flex items-center gap-2 rounded px-1 py-0.5 text-sm hover:bg-muted/30"
                  >
                    <input
                      type="checkbox"
                      checked={checked}
                      onChange={() => toggleField(f)}
                    />
                    <span>{f}</span>
                  </label>
                );
              })}
            </div>
          </div>

          {serverError && (
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">{serverError.code}</div>
                <div className="text-xs text-destructive/80">{serverError.message}</div>
              </div>
            </div>
          )}
        </div>

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => onOpenChange(false)}
            disabled={saving}
          >
            Cancel
          </Button>
          <Button type="button" size="sm" onClick={() => void handleSubmit()} disabled={saving}>
            {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
            {isNew ? 'Create' : 'Save'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
