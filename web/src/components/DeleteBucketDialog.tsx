import { useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { AlertCircle, AlertTriangle, Loader2 } from 'lucide-react';

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
  deleteBucket,
  fetchForceEmptyJob,
  startForceEmpty,
  type AdminApiError,
  type ForceEmptyJob,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  bucketName: string;
  // objectCount lets the dialog decide whether to show the force-empty CTA
  // up-front. Pass 0 for an empty bucket; the dialog hides the force-empty
  // controls in that case.
  objectCount: number;
}

const POLL_INTERVAL_MS = 750;

// DeleteBucketDialog implements US-002. Operator types the bucket name to
// arm the destructive button. When the bucket is non-empty, the dialog
// surfaces a "Force delete" toggle that runs the per-bucket drain job
// (POST /admin/v1/buckets/{bucket}/force-empty) and polls until State=done,
// then issues the regular DELETE.
export function DeleteBucketDialog({
  open,
  onOpenChange,
  bucketName,
  objectCount,
}: Props) {
  const navigate = useNavigate();
  const [confirm, setConfirm] = useState('');
  const [forceEmpty, setForceEmpty] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [job, setJob] = useState<ForceEmptyJob | null>(null);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(null);
  const cancelledRef = useRef(false);

  useEffect(() => {
    if (open) {
      setConfirm('');
      setForceEmpty(false);
      setSubmitting(false);
      setJob(null);
      setServerError(null);
      cancelledRef.current = false;
    } else {
      cancelledRef.current = true;
    }
  }, [open]);

  const isEmpty = objectCount <= 0;
  const requiresForceEmpty = !isEmpty && !forceEmpty;
  const confirmMatches = confirm === bucketName;
  const submitDisabled =
    submitting || !confirmMatches || requiresForceEmpty;

  function handleClose(next: boolean) {
    if (submitting) return; // don't let ESC kill an in-flight delete
    onOpenChange(next);
  }

  async function pollUntilDone(jobID: string): Promise<ForceEmptyJob> {
    while (!cancelledRef.current) {
      const j = await fetchForceEmptyJob(bucketName, jobID);
      setJob(j);
      if (j.state === 'done') return j;
      if (j.state === 'error') {
        throw new Error(j.message || 'force-empty failed');
      }
      await new Promise((resolve) => setTimeout(resolve, POLL_INTERVAL_MS));
    }
    throw new Error('cancelled');
  }

  async function handleDelete(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    setServerError(null);
    setSubmitting(true);
    try {
      if (forceEmpty) {
        const initial = await startForceEmpty(bucketName);
        setJob(initial);
        await pollUntilDone(initial.job_id);
      }
      await deleteBucket(bucketName);
      void queryClient.invalidateQueries({ queryKey: ['buckets', 'list'] });
      void queryClient.invalidateQueries({ queryKey: ['buckets', 'top'] });
      void queryClient.invalidateQueries({ queryKey: queryKeys.buckets.one(bucketName) });
      showToast({
        title: `Bucket ${bucketName} deleted`,
        description: forceEmpty
          ? `Force-emptied ${job?.deleted ?? 0} objects before deletion.`
          : undefined,
      });
      onOpenChange(false);
      navigate('/buckets');
    } catch (err) {
      const e = err as AdminApiError | Error;
      const code = (e as AdminApiError).code ?? 'Error';
      setServerError({ code, message: e.message });
    } finally {
      setSubmitting(false);
    }
  }

  const drainProgress = job
    ? objectCount > 0
      ? Math.min(100, Math.round((job.deleted / objectCount) * 100))
      : 0
    : 0;

  return (
    <Dialog open={open} onOpenChange={handleClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete bucket</DialogTitle>
          <DialogDescription>
            This action is irreversible. The bucket and its metadata are
            removed from the cluster.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleDelete} className="space-y-4">
          {!isEmpty && (
            <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-sm text-amber-700 dark:text-amber-300">
              <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">Bucket is not empty</div>
                <div className="text-xs">
                  {objectCount.toLocaleString()} object
                  {objectCount === 1 ? '' : 's'} remain. Tick "Force delete" to
                  drain them before deletion.
                </div>
              </div>
            </div>
          )}
          {!isEmpty && (
            <label className="inline-flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={forceEmpty}
                onChange={(e) => setForceEmpty(e.target.checked)}
                disabled={submitting}
              />
              Force delete (run drain job)
            </label>
          )}

          <div className="space-y-1.5">
            <Label htmlFor="db-confirm">
              Type <code className="font-mono text-xs">{bucketName}</code> to
              confirm
            </Label>
            <Input
              id="db-confirm"
              autoFocus
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              placeholder={bucketName}
              disabled={submitting}
            />
          </div>

          {job && (
            <div className="space-y-1.5">
              <div className="flex items-center justify-between text-xs">
                <span className="text-muted-foreground">
                  Drain {job.state}
                </span>
                <span className="tabular-nums">
                  {job.deleted.toLocaleString()} / {objectCount.toLocaleString()}
                </span>
              </div>
              <div className="h-2 overflow-hidden rounded-full bg-muted">
                <div
                  className="h-full bg-primary transition-[width] duration-300"
                  style={{ width: `${drainProgress}%` }}
                />
              </div>
            </div>
          )}

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
              onClick={() => handleClose(false)}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button type="submit" variant="destructive" disabled={submitDisabled}>
              {submitting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              {forceEmpty ? 'Force delete' : 'Delete'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
