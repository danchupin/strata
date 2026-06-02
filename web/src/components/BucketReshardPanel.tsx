import { useEffect, useMemo, useRef, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, CheckCircle2, Info, Loader2, Split } from 'lucide-react';

import {
  fetchBucketReshard,
  nextPowerOfTwo,
  startBucketReshard,
  type BucketDetail,
  type BucketReshard,
} from '@/api/client';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
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
import { cn } from '@/lib/utils';

interface Props {
  bucket: BucketDetail;
}

// Poll fast while a job is in flight so the operator watches it converge in
// near real time; idle state is cheap to refresh so we keep the same cadence.
const RESHARD_POLL_MS = 2_000;

// The tooltip the disabled action carries on a range-scan backend. Kept as a
// literal so the Playwright spec can assert the exact copy.
export const RESHARD_UNSUPPORTED_TOOLTIP =
  'range-scan backend needs no resharding';

function formatElapsed(sinceSec: number): string {
  if (!Number.isFinite(sinceSec) || sinceSec <= 0) return '0s';
  const sec = Math.floor(sinceSec);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ${sec % 60}s`;
  const hr = Math.floor(min / 60);
  return `${hr}h ${min % 60}m`;
}

export function BucketReshardPanel({ bucket }: Props) {
  const [dialogOpen, setDialogOpen] = useState(false);
  // wasActive flips true the first time we observe a queued/running job so an
  // idle observation afterwards renders the "complete" affordance rather than
  // the plain steady-state — mirrors trigger → in-progress → complete.
  const [wasActive, setWasActive] = useState(false);

  const q = useQuery<BucketReshard>({
    queryKey: queryKeys.buckets.reshard(bucket.name),
    queryFn: () => fetchBucketReshard(bucket.name),
    refetchInterval: RESHARD_POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'reshard status', silent: true },
  });

  const data = q.data;
  const active = data?.state === 'queued' || data?.state === 'running';

  useEffect(() => {
    if (active) setWasActive(true);
  }, [active]);

  const supported = data?.supported ?? false;
  const shardCount = data?.shard_count ?? bucket.shard_count;
  const justCompleted = wasActive && data?.state === 'idle';

  return (
    <Card data-testid="reshard-panel">
      <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
        <div className="space-y-1">
          <CardTitle className="text-base">Resharding</CardTitle>
          <CardDescription>
            <span data-testid="reshard-shard-count" className="tabular-nums">
              {shardCount}
            </span>{' '}
            shard{shardCount === 1 ? '' : 's'} · online reshard rewrites every
            object row into the new partition layout.
          </CardDescription>
        </div>
        <ReshardAction
          supported={supported}
          disabled={active}
          onClick={() => setDialogOpen(true)}
        />
      </CardHeader>
      <CardContent className="space-y-3">
        {!supported && (
          <div
            data-testid="reshard-noop-note"
            className="flex items-start gap-2 rounded-md border border-border/60 bg-muted/40 p-2 text-xs text-muted-foreground"
          >
            <Info className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            <span>
              This backend uses native ordered range scans — {RESHARD_UNSUPPORTED_TOOLTIP}.
              Triggering a reshard completes immediately as a no-op.
            </span>
          </div>
        )}

        {data && active && (
          <ReshardProgress data={data} />
        )}

        {justCompleted && (
          <div
            data-testid="reshard-complete"
            className="flex items-center gap-2 rounded-md border border-emerald-500/40 bg-emerald-500/5 p-2 text-sm text-emerald-700 dark:text-emerald-300"
          >
            <CheckCircle2 className="h-4 w-4 shrink-0" aria-hidden />
            Reshard complete — bucket is now sharded into {shardCount} partitions.
          </div>
        )}
      </CardContent>

      <ReshardDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        bucketName={bucket.name}
        currentShards={shardCount}
      />
    </Card>
  );
}

function ReshardAction({
  supported,
  disabled,
  onClick,
}: {
  supported: boolean;
  disabled: boolean;
  onClick: () => void;
}) {
  // A disabled <button> swallows hover, so the tooltip rides a wrapping span
  // (same trick the drain bar uses) — both the span title and the button
  // title carry the copy so the attribute is assertable regardless.
  const title = !supported ? RESHARD_UNSUPPORTED_TOOLTIP : undefined;
  return (
    <span
      data-testid="reshard-trigger-wrap"
      title={title}
      className="inline-flex"
    >
      <Button
        type="button"
        size="sm"
        variant="outline"
        data-testid="reshard-trigger"
        title={title}
        disabled={!supported || disabled}
        onClick={onClick}
      >
        <Split className="mr-1.5 h-3.5 w-3.5" aria-hidden />
        Reshard
      </Button>
    </span>
  );
}

function ReshardProgress({ data }: { data: BucketReshard }) {
  // No row-count totals are exposed by the US-005 progress endpoint, so the
  // bar is determinate only at the endpoints: queued shows a small primer,
  // running animates indeterminately. The watermark cursor + elapsed time are
  // the honest, server-backed progress signals we surface.
  const running = data.state === 'running';
  const elapsed =
    data.started_at && data.started_at > 0
      ? formatElapsed(Date.now() / 1000 - data.started_at)
      : null;
  return (
    <div className="space-y-1.5" data-testid="reshard-progress">
      <div className="flex items-center justify-between text-xs">
        <span className="inline-flex items-center gap-1.5 text-muted-foreground">
          <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
          <span data-testid="reshard-state">
            {running ? 'Migrating rows' : 'Queued'}
          </span>
          {data.source && data.target ? (
            <span className="tabular-nums">
              · {data.source} → {data.target} shards
            </span>
          ) : null}
        </span>
        {elapsed && (
          <span className="tabular-nums text-muted-foreground">
            {elapsed} elapsed
          </span>
        )}
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-muted">
        <div
          className={cn(
            'h-full bg-primary transition-[width] duration-300',
            running ? 'w-full animate-pulse' : 'w-1/12',
          )}
        />
      </div>
      {running && data.last_key ? (
        <div
          data-testid="reshard-cursor"
          className="truncate font-mono text-[11px] text-muted-foreground"
          title={data.last_key}
        >
          cursor: {data.last_key}
        </div>
      ) : null}
    </div>
  );
}

function ReshardDialog({
  open,
  onOpenChange,
  bucketName,
  currentShards,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  bucketName: string;
  currentShards: number;
}) {
  const defaultTarget = useMemo(
    () => nextPowerOfTwo(currentShards),
    [currentShards],
  );
  const [confirm, setConfirm] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const cancelledRef = useRef(false);

  useEffect(() => {
    if (open) {
      setConfirm('');
      setSubmitting(false);
      setError(null);
      cancelledRef.current = false;
    } else {
      cancelledRef.current = true;
    }
  }, [open]);

  const confirmMatches = confirm === bucketName;
  const submitDisabled = submitting || !confirmMatches;

  function handleClose(next: boolean) {
    if (submitting) return;
    onOpenChange(next);
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    setError(null);
    setSubmitting(true);
    try {
      await startBucketReshard(bucketName, defaultTarget);
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.reshard(bucketName),
      });
      showToast({
        title: `Reshard queued for ${bucketName}`,
        description: `Migrating to ${defaultTarget} shards in the background.`,
      });
      onOpenChange(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed to start reshard');
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Reshard bucket</DialogTitle>
          <DialogDescription>
            Doubles the shard count from{' '}
            <span className="font-mono">{currentShards}</span> to{' '}
            <span className="font-mono" data-testid="reshard-target">
              {defaultTarget}
            </span>
            . The reshard runs online — reads and writes keep working while
            rows migrate in the background. This is irreversible.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="reshard-confirm">
              Type <code className="font-mono text-xs">{bucketName}</code> to
              confirm
            </Label>
            <Input
              id="reshard-confirm"
              data-testid="reshard-confirm-input"
              autoFocus
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              placeholder={bucketName}
              disabled={submitting}
            />
          </div>

          {error && (
            <div
              data-testid="reshard-error"
              className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive"
            >
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div className="text-xs text-destructive/90">{error}</div>
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
            <Button
              type="submit"
              data-testid="reshard-confirm-submit"
              disabled={submitDisabled}
            >
              {submitting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Start reshard
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export default BucketReshardPanel;
