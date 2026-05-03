import { useEffect, useState } from 'react';
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
import { deleteIAMAccessKey, type AdminApiError } from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  userName: string;
  accessKeyID: string | null;
  onOpenChange: (open: boolean) => void;
}

// DeleteAccessKeyDialog requires the operator to type the access-key suffix
// (last 8 chars) before the destructive button enables. Mirrors the
// type-to-confirm bucket-delete pattern but on a key shape.
export function DeleteAccessKeyDialog({ open, userName, accessKeyID, onOpenChange }: Props) {
  const [confirmTail, setConfirmTail] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(null);

  useEffect(() => {
    if (open) {
      setConfirmTail('');
      setSubmitting(false);
      setServerError(null);
    }
  }, [open]);

  if (!accessKeyID) return null;
  const tail = accessKeyID.slice(-8);
  const matches = confirmTail.toUpperCase() === tail.toUpperCase();
  const submitDisabled = submitting || !matches;

  async function handleDelete() {
    if (!accessKeyID || submitDisabled) return;
    setServerError(null);
    setSubmitting(true);
    try {
      await deleteIAMAccessKey(accessKeyID);
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.accessKeys(userName) });
      void queryClient.invalidateQueries({ queryKey: ['iam', 'users'] });
      showToast({
        title: 'Access key deleted',
        description: accessKeyID,
      });
      onOpenChange(false);
    } catch (err) {
      const e = err as AdminApiError | Error;
      const code = (e as AdminApiError).code ?? 'Error';
      setServerError({ code, message: e.message });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete access key</DialogTitle>
          <DialogDescription>
            This permanently deletes access key{' '}
            <code className="rounded bg-muted px-1 font-mono text-xs">{accessKeyID}</code>.
            Workloads signing requests with this key will start receiving 403
            errors immediately.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="dak-confirm">
              Type the last 8 characters{' '}
              <code className="rounded bg-muted px-1">{tail}</code> to confirm
            </Label>
            <Input
              id="dak-confirm"
              autoFocus
              value={confirmTail}
              onChange={(e) => setConfirmTail(e.target.value)}
              disabled={submitting}
            />
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
            onClick={() => onOpenChange(false)}
            disabled={submitting}
          >
            Cancel
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={handleDelete}
            disabled={submitDisabled}
          >
            {submitting && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
            Delete access key
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
