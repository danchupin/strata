import { useEffect, useState } from 'react';
import { AlertCircle, Check, Copy, Loader2, ShieldAlert } from 'lucide-react';

import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Label } from '@/components/ui/label';
import {
  createIAMAccessKey,
  type AdminApiError,
  type IAMAccessKeyCreateResponse,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  userName: string;
  onOpenChange: (open: boolean) => void;
}

// CreateAccessKeyDialog mints a fresh access-key + secret pair and renders
// the pair as a copy-once panel. The secret cannot be re-fetched after the
// dialog closes — banner is copy: 'This is the only time the secret will be
// shown'.
export function CreateAccessKeyDialog({ open, userName, onOpenChange }: Props) {
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(null);
  const [created, setCreated] = useState<IAMAccessKeyCreateResponse | null>(null);
  const [copiedField, setCopiedField] = useState<'id' | 'secret' | null>(null);

  useEffect(() => {
    if (open) {
      setSubmitting(false);
      setServerError(null);
      setCreated(null);
      setCopiedField(null);
    }
  }, [open]);

  async function handleCreate() {
    setServerError(null);
    setSubmitting(true);
    try {
      const resp = await createIAMAccessKey(userName);
      setCreated(resp);
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.accessKeys(userName) });
      void queryClient.invalidateQueries({ queryKey: ['iam', 'users'] });
      showToast({
        title: 'Access key created',
        description: 'Copy the secret now — it will not be shown again.',
      });
    } catch (err) {
      const e = err as AdminApiError | Error;
      const code = (e as AdminApiError).code ?? 'Error';
      setServerError({ code, message: e.message });
    } finally {
      setSubmitting(false);
    }
  }

  async function copy(value: string, field: 'id' | 'secret') {
    try {
      await navigator.clipboard.writeText(value);
      setCopiedField(field);
      setTimeout(() => setCopiedField((cur) => (cur === field ? null : cur)), 1500);
    } catch {
      // Some browsers reject clipboard writes outside HTTPS — surface a hint.
      showToast({
        title: 'Clipboard unavailable',
        description: 'Select the value manually to copy.',
        variant: 'destructive',
      });
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Create access key</DialogTitle>
          <DialogDescription>
            Mint a new access key + secret pair for{' '}
            <strong>{userName}</strong>.
          </DialogDescription>
        </DialogHeader>
        {!created && (
          <div className="space-y-3 text-sm text-muted-foreground">
            <p>
              The secret access key is shown only once after creation. Strata
              cannot recover it later — copy it into a credentials store
              before closing this dialog.
            </p>
            {serverError && (
              <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-destructive">
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                <div>
                  <div className="font-medium">{serverError.code}</div>
                  <div className="text-xs text-destructive/80">{serverError.message}</div>
                </div>
              </div>
            )}
          </div>
        )}
        {created && (
          <div className="space-y-3">
            <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/5 p-2 text-sm text-amber-700 dark:text-amber-300">
              <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">
                  This is the only time the secret will be shown. Copy it now.
                </div>
              </div>
            </div>
            <div className="space-y-1.5">
              <Label>Access key ID</Label>
              <div className="flex items-center gap-2">
                <code className="flex-1 rounded bg-muted px-2 py-1 font-mono text-xs">
                  {created.access_key_id}
                </code>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => copy(created.access_key_id, 'id')}
                >
                  {copiedField === 'id' ? (
                    <Check className="h-3.5 w-3.5" aria-hidden />
                  ) : (
                    <Copy className="h-3.5 w-3.5" aria-hidden />
                  )}
                </Button>
              </div>
            </div>
            <div className="space-y-1.5">
              <Label>Secret access key</Label>
              <div className="flex items-center gap-2">
                <code className="flex-1 break-all rounded bg-muted px-2 py-1 font-mono text-xs">
                  {created.secret_access_key}
                </code>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => copy(created.secret_access_key, 'secret')}
                >
                  {copiedField === 'secret' ? (
                    <Check className="h-3.5 w-3.5" aria-hidden />
                  ) : (
                    <Copy className="h-3.5 w-3.5" aria-hidden />
                  )}
                </Button>
              </div>
            </div>
          </div>
        )}
        <DialogFooter>
          {!created && (
            <>
              <Button
                type="button"
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                Cancel
              </Button>
              <Button type="button" onClick={handleCreate} disabled={submitting}>
                {submitting && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
                Create access key
              </Button>
            </>
          )}
          {created && (
            <Button type="button" onClick={() => onOpenChange(false)}>
              Done
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
