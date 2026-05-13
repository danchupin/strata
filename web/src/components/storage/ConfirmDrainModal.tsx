import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, AlertTriangle, Loader2 } from 'lucide-react';

import { drainCluster, fetchClusterBucketReferences } from '@/api/client';
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
  // onShowReferences opens the affected-buckets drawer WITHOUT closing the
  // modal so the operator can review impact, dismiss the drawer, and still
  // complete the typed-confirmation flow.
  onShowReferences?: () => void;
}

// ConfirmDrainModal mirrors DeleteBucketDialog's typed-confirmation
// pattern: the destructive submit stays disabled until the operator
// types the cluster id verbatim (case-sensitive). Drain is reversible
// via Undrain — copy makes that explicit so the friction reads as
// "double-check the right cluster" rather than "irreversible".
export function ConfirmDrainModal({ open, onOpenChange, clusterID, onShowReferences }: Props) {
  const [typed, setTyped] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);

  // Cheap impact preview: fetch the first page of bucket-references when the
  // modal opens so we can surface the total count inline. The drawer reuses
  // the same query key on first render.
  const refsQ = useQuery({
    queryKey: queryKeys.clusterBucketRefs(clusterID, 100, 0),
    queryFn: () => fetchClusterBucketReferences(clusterID, 100, 0),
    enabled: open,
    staleTime: 30_000,
    meta: { silent: true },
  });
  const refsTotal = refsQ.data?.total_buckets;

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
      await drainCluster(clusterID);
      showToast({
        title: `Draining cluster ${clusterID}`,
        description:
          'PUTs will route around it once the drain cache TTL elapses; the rebalance worker is moving existing chunks off.',
      });
      void queryClient.invalidateQueries({ queryKey: queryKeys.clusters });
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
          <DialogTitle>Drain cluster</DialogTitle>
          <DialogDescription>
            Draining stops new PUTs from landing on this cluster and queues
            existing chunks for migration to a peer. The flip is reversible —
            press Undrain to restore live state.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-sm text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Confirm cluster id</div>
              <div className="text-xs">
                Type <code className="font-mono">{clusterID}</code> exactly
                to arm the Drain button.
              </div>
            </div>
          </div>

          {refsTotal != null && (
            <div className="rounded-md border bg-muted/30 p-2 text-xs text-muted-foreground">
              <span className="font-medium text-foreground">{refsTotal}</span>{' '}
              {refsTotal === 1 ? 'bucket references' : 'buckets reference'}{' '}
              this cluster in their Placement policy
              {onShowReferences && refsTotal > 0 && (
                <>
                  {' — '}
                  <button
                    type="button"
                    onClick={onShowReferences}
                    className="text-primary hover:underline"
                  >
                    view list
                  </button>
                </>
              )}
            </div>
          )}

          <div className="space-y-1.5">
            <Label htmlFor="cd-confirm">
              Cluster id
            </Label>
            <Input
              id="cd-confirm"
              autoFocus
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              placeholder={clusterID}
              disabled={submitting}
              autoComplete="off"
              autoCorrect="off"
              spellCheck={false}
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
              Cancel
            </Button>
            <Button type="submit" variant="destructive" disabled={submitDisabled}>
              {submitting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Drain
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
