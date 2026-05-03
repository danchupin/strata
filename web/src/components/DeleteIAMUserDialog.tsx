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
import { deleteIAMUser, type AdminApiError, type IAMUserSummary } from '@/api/client';
import { queryClient } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  user: IAMUserSummary | null;
  onOpenChange: (open: boolean) => void;
}

// DeleteIAMUserDialog cascades access-key deletion server-side. Required type-
// to-confirm guard mirrors the bucket-delete dialog pattern (US-002).
export function DeleteIAMUserDialog({ open, user, onOpenChange }: Props) {
  const [confirmName, setConfirmName] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(null);

  useEffect(() => {
    if (open) {
      setConfirmName('');
      setSubmitting(false);
      setServerError(null);
    }
  }, [open]);

  if (!user) return null;

  const matches = confirmName === user.user_name;
  const submitDisabled = submitting || !matches;

  async function handleDelete() {
    if (!user || submitDisabled) return;
    setServerError(null);
    setSubmitting(true);
    try {
      await deleteIAMUser(user.user_name);
      void queryClient.invalidateQueries({ queryKey: ['iam', 'users'] });
      showToast({
        title: `User ${user.user_name} deleted`,
        description:
          user.access_key_count > 0
            ? `${user.access_key_count} access key(s) cascaded`
            : undefined,
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
          <DialogTitle>Delete IAM user</DialogTitle>
          <DialogDescription>
            This permanently deletes user <strong>{user.user_name}</strong>.
            {user.access_key_count > 0 && (
              <>
                {' '}
                {user.access_key_count} access key
                {user.access_key_count === 1 ? '' : 's'} owned by this user
                will be deleted alongside.
              </>
            )}
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="iud-confirm">
              Type <code className="rounded bg-muted px-1">{user.user_name}</code> to confirm
            </Label>
            <Input
              id="iud-confirm"
              autoFocus
              value={confirmName}
              onChange={(e) => setConfirmName(e.target.value)}
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
            Delete user
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
