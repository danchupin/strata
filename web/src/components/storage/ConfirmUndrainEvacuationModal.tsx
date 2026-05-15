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
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  clusterID: string;
}

// ConfirmUndrainEvacuationModal fires when an operator clicks the
// "Undrain (cancel evacuation)" button on a mid-flight evacuating
// cluster (chunks_on_cluster > 0). The modal does NOT use typed-confirm
// — the operation is recoverable (re-issue Drain) so a one-click yes/no
// matches the existing UX. The message stresses that already-moved
// chunks REMAIN on their target clusters; the rebalance worker won't
// pull them back (US-007 drain-cleanup truth table cell:
// evacuating + chunks > 0).
export function ConfirmUndrainEvacuationModal({
  open,
  onOpenChange,
  clusterID,
}: Props) {
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setSubmitting(false);
      setServerError(null);
    }
  }, [open]);

  function handleClose(next: boolean) {
    if (submitting) return;
    onOpenChange(next);
  }

  async function handleSubmit() {
    setServerError(null);
    setSubmitting(true);
    try {
      await undrainCluster(clusterID);
      showToast({
        title: `Evacuation cancelled on cluster ${clusterID}`,
        description:
          'Already-moved chunks remain on their target clusters; no rollback.',
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
          <DialogTitle>Undrain (cancel evacuation)</DialogTitle>
          <DialogDescription>
            Cluster {clusterID} is mid-evacuation. Cancelling now restores
            it to <code>live</code> so it can accept writes again.
          </DialogDescription>
        </DialogHeader>
        <div
          className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-sm text-amber-800 dark:text-amber-300"
          data-testid="cue-warning"
        >
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
          <div className="space-y-1">
            <div className="font-medium">No rollback</div>
            <div className="text-xs leading-snug">
              Moved chunks remain on target clusters; no rollback. Cluster
              will accept writes again.
            </div>
          </div>
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
            type="button"
            variant="destructive"
            onClick={handleSubmit}
            disabled={submitting}
            data-testid="cue-submit"
          >
            {submitting && (
              <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
            )}
            Undrain (cancel evacuation)
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
