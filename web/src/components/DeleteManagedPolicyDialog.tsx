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
import {
  deleteManagedPolicy,
  type AdminApiError,
  type ManagedPolicySummary,
  type PolicyAttachedError,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  policy: ManagedPolicySummary | null;
  onOpenChange: (open: boolean) => void;
}

export function DeleteManagedPolicyDialog({ open, policy, onOpenChange }: Props) {
  const [confirm, setConfirm] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );
  const [attachedTo, setAttachedTo] = useState<string[] | null>(null);

  useEffect(() => {
    if (open) {
      setConfirm('');
      setSubmitting(false);
      setServerError(null);
      setAttachedTo(null);
    }
  }, [open, policy?.arn]);

  if (!policy) return null;
  const matched = confirm.trim() === policy.name;
  const submitDisabled = submitting || !matched;

  async function handleDelete() {
    if (!policy) return;
    setSubmitting(true);
    setServerError(null);
    setAttachedTo(null);
    try {
      await deleteManagedPolicy(policy.arn);
      showToast({ title: `Policy ${policy.name} deleted` });
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.policies });
      onOpenChange(false);
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
      if ((e as PolicyAttachedError).code === 'PolicyAttached') {
        setAttachedTo((e as PolicyAttachedError).attachedTo ?? []);
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete managed policy</DialogTitle>
          <DialogDescription>
            This removes <code>{policy.arn}</code>. Type the policy name to
            confirm.
          </DialogDescription>
        </DialogHeader>
        {policy.attachment_count > 0 && attachedTo === null && (
          <div className="rounded-md border border-amber-500/40 bg-amber-500/5 p-2 text-sm text-amber-700 dark:text-amber-400">
            <div className="font-medium">Attached to {policy.attachment_count} user(s)</div>
            <div className="text-xs">
              Detach this policy from every user before deleting (US-014).
            </div>
          </div>
        )}
        <div className="space-y-2">
          <Label htmlFor="mp-delete-confirm">Policy name</Label>
          <Input
            id="mp-delete-confirm"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            placeholder={policy.name}
            autoComplete="off"
            disabled={submitting}
          />
        </div>
        {serverError && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div className="space-y-1">
              <div className="font-medium">{serverError.code}</div>
              <div className="text-xs text-destructive/80">{serverError.message}</div>
              {attachedTo !== null && attachedTo.length > 0 && (
                <ul className="list-inside list-disc text-xs text-destructive/80">
                  {attachedTo.map((u) => (
                    <li key={u}>
                      <code>{u}</code>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>
        )}
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => onOpenChange(false)}
            disabled={submitting}
          >
            Cancel
          </Button>
          <Button
            type="button"
            variant="destructive"
            size="sm"
            onClick={handleDelete}
            disabled={submitDisabled}
          >
            {submitting && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
            Delete policy
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
