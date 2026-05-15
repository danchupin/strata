import { useEffect, useState } from 'react';
import { AlertCircle, AlertTriangle, Loader2 } from 'lucide-react';

import { undrainCluster } from '@/api/client';
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
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  clusterID: string;
}

// CancelDeregisterPrepModal is the typed-confirm gate that fires when an
// operator wants to roll a fully-evacuated cluster back to `live` (e.g.
// they changed their mind about deregistering). Mirrors ConfirmDrainModal's
// typed-confirm pattern: the operator must type the cluster id verbatim to
// arm Submit. On success calls POST /admin/v1/clusters/{id}/undrain which
// flips state→live. The toast wording emphasises that previously-migrated
// chunks STAY on their target clusters (US-007 drain-cleanup truth table
// cell: evacuating + chunks=0 + deregister_ready=true).
export function CancelDeregisterPrepModal({
  open,
  onOpenChange,
  clusterID,
}: Props) {
  const [typed, setTyped] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setTyped('');
      setSubmitting(false);
      setServerError(null);
    }
  }, [open]);

  const matches = typed === clusterID;
  const submitDisabled = submitting || !matches;

  function handleClose(next: boolean) {
    if (submitting) return;
    onOpenChange(next);
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    setServerError(null);
    setSubmitting(true);
    try {
      await undrainCluster(clusterID);
      showToast({
        title: `Cluster ${clusterID} restored to live`,
        description: 'No chunks restored — migrated chunks stay on their target clusters.',
      });
      void queryClient.invalidateQueries({ queryKey: queryKeys.clusters });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.clusterDrainProgress(clusterID),
      });
      onOpenChange(false);
    } catch (err) {
      setServerError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Cancel deregister prep</DialogTitle>
          <DialogDescription>
            Cluster {clusterID} has finished evacuating and is ready to
            deregister. Cancelling restores it to <code>live</code> so it
            can accept writes again. Migrated chunks stay on their new
            clusters — nothing is rolled back.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div
            className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-sm text-amber-700 dark:text-amber-300"
            data-testid="cdp-warning"
          >
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Confirm cluster id</div>
              <div className="text-xs">
                Type <code className="font-mono">{clusterID}</code> exactly
                to arm the Cancel deregister prep button.
              </div>
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="cdp-confirm">Cluster id</Label>
            <Input
              id="cdp-confirm"
              autoFocus
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              placeholder={clusterID}
              disabled={submitting}
              autoComplete="off"
              autoCorrect="off"
              spellCheck={false}
              data-testid="cdp-input"
            />
          </div>

          {serverError && (
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div className="text-xs">{serverError}</div>
            </div>
          )}

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => handleClose(false)}
              disabled={submitting}
            >
              Keep evacuating
            </Button>
            <Button
              type="submit"
              variant="destructive"
              disabled={submitDisabled}
              data-testid="cdp-submit"
            >
              {submitting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Cancel deregister prep
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
