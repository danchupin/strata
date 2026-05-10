import { useEffect, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { AlertCircle, Loader2 } from 'lucide-react';

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
import {
  setBucketQuota,
  type AdminApiError,
  type BucketQuota,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  bucket: string;
  current: BucketQuota | null;
}

// EditBucketQuotaDialog gates the per-bucket BucketQuota PUT (US-010).
// Zero on any field means "unlimited" — the helper text + placeholder make
// that explicit so the operator never needs to memorise the convention.
export function EditBucketQuotaDialog({ open, onOpenChange, bucket, current }: Props) {
  const [maxBytes, setMaxBytes] = useState('');
  const [maxObjects, setMaxObjects] = useState('');
  const [maxBytesPerObject, setMaxBytesPerObject] = useState('');
  const [error, setError] = useState<{ code: string; message: string } | null>(null);

  useEffect(() => {
    if (open) {
      setMaxBytes(current?.max_bytes ? String(current.max_bytes) : '');
      setMaxObjects(current?.max_objects ? String(current.max_objects) : '');
      setMaxBytesPerObject(
        current?.max_bytes_per_object ? String(current.max_bytes_per_object) : '',
      );
      setError(null);
    }
  }, [open, current]);

  const mutate = useMutation({
    mutationFn: (q: BucketQuota) => setBucketQuota(bucket, q),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.buckets.quota(bucket) });
      showToast({ title: 'Quota saved', description: bucket });
      onOpenChange(false);
    },
    onError: (err) => {
      const e = err as AdminApiError | Error;
      const code = (e as AdminApiError).code ?? 'Error';
      setError({ code, message: e.message });
    },
  });

  function parseField(s: string): number | null {
    const t = s.trim();
    if (!t) return 0;
    const n = Number(t);
    if (!Number.isFinite(n) || n < 0 || !Number.isInteger(n)) return null;
    return n;
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    const mb = parseField(maxBytes);
    const mo = parseField(maxObjects);
    const mbpo = parseField(maxBytesPerObject);
    if (mb === null || mo === null || mbpo === null) {
      setError({
        code: 'InvalidArgument',
        message: 'fields must be non-negative integers (zero = unlimited)',
      });
      return;
    }
    mutate.mutate({
      max_bytes: mb,
      max_objects: mo,
      max_bytes_per_object: mbpo,
    });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Edit bucket quota</DialogTitle>
          <DialogDescription>
            Hard-cap bucket usage. Zero means unlimited. Writes that exceed
            any cap return <code>403 QuotaExceeded</code>.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="quota-maxbytes">Max bytes</Label>
            <Input
              id="quota-maxbytes"
              inputMode="numeric"
              value={maxBytes}
              onChange={(e) => setMaxBytes(e.target.value)}
              placeholder="0 = unlimited"
              disabled={mutate.isPending}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="quota-maxobjects">Max objects</Label>
            <Input
              id="quota-maxobjects"
              inputMode="numeric"
              value={maxObjects}
              onChange={(e) => setMaxObjects(e.target.value)}
              placeholder="0 = unlimited"
              disabled={mutate.isPending}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="quota-mbpo">Max bytes per object</Label>
            <Input
              id="quota-mbpo"
              inputMode="numeric"
              value={maxBytesPerObject}
              onChange={(e) => setMaxBytesPerObject(e.target.value)}
              placeholder="0 = unlimited"
              disabled={mutate.isPending}
            />
          </div>
          {error && (
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">{error.code}</div>
                <div className="text-xs text-destructive/80">{error.message}</div>
              </div>
            </div>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={mutate.isPending}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={mutate.isPending}>
              {mutate.isPending && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Save quota
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
