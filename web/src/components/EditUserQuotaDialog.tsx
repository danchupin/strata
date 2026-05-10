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
  setUserQuota,
  type AdminApiError,
  type UserQuota,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  userName: string;
  current: UserQuota | null;
}

// EditUserQuotaDialog gates the per-user UserQuota PUT (US-010).
// Mirrors EditBucketQuotaDialog — zero ⇒ unlimited.
export function EditUserQuotaDialog({ open, onOpenChange, userName, current }: Props) {
  const [maxBuckets, setMaxBuckets] = useState('');
  const [totalMaxBytes, setTotalMaxBytes] = useState('');
  const [error, setError] = useState<{ code: string; message: string } | null>(null);

  useEffect(() => {
    if (open) {
      setMaxBuckets(current?.max_buckets ? String(current.max_buckets) : '');
      setTotalMaxBytes(
        current?.total_max_bytes ? String(current.total_max_bytes) : '',
      );
      setError(null);
    }
  }, [open, current]);

  const mutate = useMutation({
    mutationFn: (q: UserQuota) => setUserQuota(userName, q),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.userQuota(userName) });
      showToast({ title: 'User quota saved', description: userName });
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
    const mb = parseField(maxBuckets);
    const tmb = parseField(totalMaxBytes);
    if (mb === null || tmb === null) {
      setError({
        code: 'InvalidArgument',
        message: 'fields must be non-negative integers (zero = unlimited)',
      });
      return;
    }
    mutate.mutate({ max_buckets: mb, total_max_bytes: tmb });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Edit user quota</DialogTitle>
          <DialogDescription>
            Hard-cap per-user totals across every bucket the user owns. Zero
            means unlimited.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="uquota-maxbuckets">Max buckets</Label>
            <Input
              id="uquota-maxbuckets"
              inputMode="numeric"
              value={maxBuckets}
              onChange={(e) => setMaxBuckets(e.target.value)}
              placeholder="0 = unlimited"
              disabled={mutate.isPending}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="uquota-totalbytes">Total max bytes</Label>
            <Input
              id="uquota-totalbytes"
              inputMode="numeric"
              value={totalMaxBytes}
              onChange={(e) => setTotalMaxBytes(e.target.value)}
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
