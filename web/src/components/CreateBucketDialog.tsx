import { useEffect, useMemo, useState } from 'react';
import { AlertCircle, Loader2 } from 'lucide-react';
import { useQuery } from '@tanstack/react-query';

import { Button } from '@/components/ui/button';
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
import { fetchClusterStatus } from '@/api/cluster';
import {
  createBucket,
  type CreateBucketBody,
  type CreateBucketError,
  type BucketDetail,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

// Mirror of internal/s3api/validate.go: validBucketName. Kept client-side so
// the dialog can render inline rule violations as the operator types instead
// of a server round-trip per keystroke. Server still re-validates.
const RESERVED_NAMES = new Set([
  'console',
  'admin',
  'metrics',
  'healthz',
  'readyz',
  '.well-known',
]);

function nameViolation(raw: string): string | null {
  const name = raw.trim();
  if (name.length === 0) return null;
  if (RESERVED_NAMES.has(name)) return 'reserved gateway-internal route name';
  if (name.length < 3 || name.length > 63)
    return 'must be 3..63 characters long';
  if (!/^[a-z0-9]/.test(name))
    return 'must start with a lowercase letter or digit';
  if (!/[a-z0-9]$/.test(name))
    return 'must end with a lowercase letter or digit';
  if (/[^a-z0-9.-]/.test(name))
    return 'only lowercase letters, digits, hyphen, and dot are allowed';
  if (name.includes('..') || name.includes('.-') || name.includes('-.'))
    return 'no "..", ".-", or "-." sequences';
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(name))
    return 'must not look like an IPv4 address';
  return null;
}

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated?: (bucket: BucketDetail) => void;
}

type Versioning = 'Suspended' | 'Enabled';

export function CreateBucketDialog({ open, onOpenChange, onCreated }: Props) {
  const cluster = useQuery({
    queryKey: queryKeys.cluster.status,
    queryFn: fetchClusterStatus,
    meta: { silent: true },
    enabled: open,
  });

  const [name, setName] = useState('');
  const [region, setRegion] = useState('');
  const [versioning, setVersioning] = useState<Versioning>('Suspended');
  const [objectLock, setObjectLock] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(null);

  // Reset state every time the dialog opens, and prefill region with the
  // cluster default so the operator only types it for cross-region creation.
  useEffect(() => {
    if (open) {
      setName('');
      setVersioning('Suspended');
      setObjectLock(false);
      setSubmitting(false);
      setServerError(null);
    }
  }, [open]);

  useEffect(() => {
    if (!region && cluster.data?.region) {
      setRegion(cluster.data.region);
    }
  }, [cluster.data?.region, region]);

  const violation = useMemo(() => nameViolation(name), [name]);
  const objectLockDisabled = versioning !== 'Enabled';
  // Object-Lock greys out + forces off when versioning is Suspended; the AC
  // wants the field unchecked + uneditable in that state.
  useEffect(() => {
    if (objectLockDisabled && objectLock) setObjectLock(false);
  }, [objectLockDisabled, objectLock]);

  const submitDisabled =
    submitting || name.trim().length === 0 || violation !== null;

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    setServerError(null);
    setSubmitting(true);
    const body: CreateBucketBody = {
      name: name.trim(),
      versioning,
      object_lock_enabled: objectLock,
    };
    if (region.trim()) body.region = region.trim();
    try {
      const detail = await createBucket(body);
      // Refetch the bucket list so the new row shows up immediately.
      void queryClient.invalidateQueries({ queryKey: ['buckets', 'list'] });
      void queryClient.invalidateQueries({ queryKey: ['buckets', 'top'] });
      showToast({
        title: `Bucket ${detail.name} created`,
        description: `Region ${detail.region || '—'} · Versioning ${detail.versioning}`,
      });
      onCreated?.(detail);
      onOpenChange(false);
    } catch (err) {
      const e = err as CreateBucketError | Error;
      const code = (e as CreateBucketError).code ?? 'Error';
      setServerError({ code, message: e.message });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create bucket</DialogTitle>
          <DialogDescription>
            Create a new bucket in this cluster. Versioning and Object-Lock
            defaults can still be changed later from the bucket detail page.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="cb-name">Name</Label>
            <Input
              id="cb-name"
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-bucket"
              aria-invalid={violation ? 'true' : 'false'}
              disabled={submitting}
            />
            {violation ? (
              <p className="text-xs text-destructive">{violation}</p>
            ) : (
              <p className="text-xs text-muted-foreground">
                3–63 chars; lowercase letters, digits, hyphens, and dots; must
                start and end alphanumeric.
              </p>
            )}
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="cb-region">Region</Label>
            <Input
              id="cb-region"
              value={region}
              onChange={(e) => setRegion(e.target.value)}
              placeholder={cluster.data?.region || 'us-east-1'}
              disabled={submitting}
            />
            <p className="text-xs text-muted-foreground">
              Defaults to the cluster region
              {cluster.data?.region ? ` (${cluster.data.region})` : ''}.
            </p>
          </div>

          <fieldset className="space-y-2" disabled={submitting}>
            <legend className="text-sm font-medium">Versioning</legend>
            <div className="flex items-center gap-4 text-sm">
              <label className="inline-flex items-center gap-1.5">
                <input
                  type="radio"
                  name="cb-versioning"
                  value="Suspended"
                  checked={versioning === 'Suspended'}
                  onChange={() => setVersioning('Suspended')}
                />
                Suspended
              </label>
              <label className="inline-flex items-center gap-1.5">
                <input
                  type="radio"
                  name="cb-versioning"
                  value="Enabled"
                  checked={versioning === 'Enabled'}
                  onChange={() => setVersioning('Enabled')}
                />
                Enabled
              </label>
            </div>
          </fieldset>

          <div className="space-y-1.5">
            <label
              className={cn(
                'inline-flex items-center gap-2 text-sm',
                objectLockDisabled && 'opacity-60',
              )}
              title={
                objectLockDisabled
                  ? 'Object-Lock requires Versioning=Enabled'
                  : undefined
              }
            >
              <input
                type="checkbox"
                checked={objectLock}
                onChange={(e) => setObjectLock(e.target.checked)}
                disabled={objectLockDisabled || submitting}
              />
              Enable Object-Lock
            </label>
            {objectLockDisabled && (
              <p className="text-xs text-muted-foreground">
                Object-Lock requires Versioning = Enabled.
              </p>
            )}
          </div>

          {serverError && (
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">{serverError.code}</div>
                <div className="text-xs text-destructive/80">
                  {serverError.message}
                </div>
              </div>
            </div>
          )}

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={submitDisabled}>
              {submitting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Create bucket
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
