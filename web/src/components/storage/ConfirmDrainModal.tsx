import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import {
  AlertCircle,
  AlertTriangle,
  Ban,
  Loader2,
  Wrench,
} from 'lucide-react';

import {
  drainCluster,
  fetchClusterDrainImpact,
  type BucketImpactEntry,
  type ClusterDrainImpactResponse,
  type ClusterState,
} from '@/api/client';
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
import { cn } from '@/lib/utils';

type Mode = 'readonly' | 'evacuate';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  clusterID: string;
  // currentState is the cluster's current 4-state value; the modal flips
  // into "Upgrade to evacuate" shape when it equals draining_readonly
  // (readonly radio hidden — undrain is the path back to live).
  currentState?: ClusterState | string;
  // onOpenBulkFix opens the BulkPlacementFixDialog (US-005) with the
  // stuck bucket rows from the cached /drain-impact response. The parent
  // is responsible for rendering the dialog and refetching impact on
  // close so live counts update.
  onOpenBulkFix?: (stuck: BucketImpactEntry[]) => void;
}

// ConfirmDrainModal asks the operator to pick a drain mode, previews
// impact when the choice is evacuate, and blocks Submit when any chunk
// would be stuck — so data can never be left behind on an evacuating
// cluster. Maintenance shape (mode=readonly) skips impact analysis: a
// stop-writes drain is reversible at any time via Undrain and leaves no
// chunks behind.
export function ConfirmDrainModal({
  open,
  onOpenChange,
  clusterID,
  currentState,
  onOpenBulkFix,
}: Props) {
  const isUpgrade = currentState === 'draining_readonly';
  const [mode, setMode] = useState<Mode>(isUpgrade ? 'evacuate' : 'readonly');
  const [typed, setTyped] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setMode(isUpgrade ? 'evacuate' : 'readonly');
      setTyped('');
      setSubmitting(false);
      setServerError(null);
    }
  }, [open, isUpgrade]);

  // Impact preview is needed only for the evacuate path. The readonly
  // path is a stop-writes flip with no chunk movement, so a scan would
  // burn IO for nothing.
  const impactQ = useQuery({
    queryKey: queryKeys.clusterDrainImpact(clusterID),
    queryFn: () => fetchClusterDrainImpact(clusterID),
    enabled: open && mode === 'evacuate',
    staleTime: 30_000,
    meta: { silent: true },
  });
  const impact = impactQ.data;
  const stuckTotal = impact
    ? impact.stuck_single_policy_chunks + impact.stuck_no_policy_chunks
    : 0;
  const stuckBuckets = useMemo<BucketImpactEntry[]>(() => {
    if (!impact) return [];
    return impact.by_bucket.filter((b) => b.category !== 'migratable');
  }, [impact]);

  const matches = typed === clusterID;
  const submitDisabled =
    submitting ||
    !matches ||
    (mode === 'evacuate' && (impactQ.isPending || stuckTotal > 0));

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
      await drainCluster(clusterID, mode);
      showToast({
        title:
          mode === 'evacuate'
            ? `Evacuating cluster ${clusterID}`
            : `Stop-writes drain on cluster ${clusterID}`,
        description:
          mode === 'evacuate'
            ? 'PUTs will route around it and the rebalance worker is moving existing chunks off.'
            : 'New PUTs are refused on this cluster. Existing chunks stay put — undrain to resume writes.',
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

  const title = isUpgrade ? 'Upgrade to evacuate' : 'Drain cluster';
  const description = isUpgrade
    ? 'Cluster is already in stop-writes mode. Upgrade to full evacuate to migrate existing chunks off before deregistering.'
    : 'Pick a mode. Stop-writes is reversible at any time and leaves chunks where they are. Evacuate migrates chunks to peers so the cluster can be deregistered.';

  return (
    <Dialog open={open} onOpenChange={handleClose}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <fieldset
            className="space-y-2"
            data-testid="cd-mode-picker"
            disabled={submitting}
          >
            <legend className="text-sm font-medium">Mode</legend>
            {!isUpgrade && (
              <ModeOption
                id="cd-mode-readonly"
                label="Stop new writes (maintenance)"
                description="Refuses new PUTs to this cluster. Reads, deletes, and in-flight multipart sessions continue. Reversible via Undrain."
                value="readonly"
                selected={mode === 'readonly'}
                onSelect={() => setMode('readonly')}
              />
            )}
            <ModeOption
              id="cd-mode-evacuate"
              label="Full evacuate (decommission)"
              description="Stop-writes plus the rebalance worker migrates every chunk on this cluster to peers so the node can be deregistered."
              value="evacuate"
              selected={mode === 'evacuate'}
              onSelect={() => setMode('evacuate')}
            />
          </fieldset>

          {mode === 'evacuate' && (
            <ImpactSection
              data={impact}
              loading={impactQ.isPending}
              error={
                impactQ.error instanceof Error ? impactQ.error.message : null
              }
              stuckBuckets={stuckBuckets}
              stuckTotal={stuckTotal}
              onOpenBulkFix={onOpenBulkFix}
            />
          )}

          <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-sm text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Confirm cluster id</div>
              <div className="text-xs">
                Type <code className="font-mono">{clusterID}</code> exactly to
                arm the {mode === 'evacuate' ? 'Evacuate' : 'Drain'} button.
              </div>
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="cd-confirm">Cluster id</Label>
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
            <Button
              type="submit"
              variant="destructive"
              disabled={submitDisabled}
              data-testid="cd-submit"
            >
              {submitting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              {submitLabel(mode, impact, impactQ.isPending, stuckTotal)}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function submitLabel(
  mode: Mode,
  impact: ClusterDrainImpactResponse | undefined,
  loading: boolean,
  stuckTotal: number,
): string {
  if (mode === 'readonly') return 'Drain (stop-writes)';
  if (loading) return 'Loading impact…';
  if (!impact) return 'Evacuate';
  if (stuckTotal > 0) {
    const stuckBuckets = impact.by_bucket.filter(
      (b) => b.category !== 'migratable',
    ).length;
    return `Drain blocked — fix ${stuckBuckets} stuck ${
      stuckBuckets === 1 ? 'bucket' : 'buckets'
    }`;
  }
  return `Drain (${impact.migratable_chunks.toLocaleString()} chunks will migrate)`;
}

interface ModeOptionProps {
  id: string;
  label: string;
  description: string;
  value: Mode;
  selected: boolean;
  onSelect: () => void;
}

function ModeOption({
  id,
  label,
  description,
  value,
  selected,
  onSelect,
}: ModeOptionProps) {
  return (
    <label
      htmlFor={id}
      className={cn(
        'flex cursor-pointer items-start gap-2 rounded-md border p-2 text-sm transition-colors',
        selected
          ? 'border-primary bg-primary/5'
          : 'border-border hover:bg-muted/40',
      )}
    >
      <input
        id={id}
        type="radio"
        name="cd-mode"
        value={value}
        checked={selected}
        onChange={onSelect}
        className="mt-1 h-3.5 w-3.5"
        data-testid={id}
      />
      <span className="space-y-0.5">
        <span className="block font-medium leading-tight">{label}</span>
        <span className="block text-xs text-muted-foreground">
          {description}
        </span>
      </span>
    </label>
  );
}

interface ImpactSectionProps {
  data: ClusterDrainImpactResponse | undefined;
  loading: boolean;
  error: string | null;
  stuckBuckets: BucketImpactEntry[];
  stuckTotal: number;
  onOpenBulkFix?: (stuck: BucketImpactEntry[]) => void;
}

function ImpactSection({
  data,
  loading,
  error,
  stuckBuckets,
  stuckTotal,
  onOpenBulkFix,
}: ImpactSectionProps) {
  if (loading) {
    return (
      <div
        className="flex items-center gap-2 rounded-md border bg-muted/30 p-2 text-xs text-muted-foreground"
        data-testid="cd-impact-loading"
      >
        <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
        Analyzing impact…
      </div>
    );
  }
  if (error) {
    return (
      <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-xs text-destructive">
        <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
        <div>
          <div className="font-medium">Impact analysis failed</div>
          <div>{error}</div>
        </div>
      </div>
    );
  }
  if (!data) return null;
  const stuckBucketCount = stuckBuckets.length;
  return (
    <div className="space-y-2" data-testid="cd-impact">
      <div className="grid grid-cols-3 gap-2 text-center text-xs">
        <ImpactCounter
          tone="ok"
          label="Migratable"
          value={data.migratable_chunks}
        />
        <ImpactCounter
          tone="stuck"
          label="Stuck (single policy)"
          value={data.stuck_single_policy_chunks}
        />
        <ImpactCounter
          tone="stuck"
          label="Stuck (no policy)"
          value={data.stuck_no_policy_chunks}
        />
      </div>
      {stuckTotal > 0 ? (
        <div
          className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-800 dark:text-amber-300"
          data-testid="cd-stuck-warning"
        >
          <Ban className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          <div className="space-y-1">
            <div className="font-medium">
              {stuckTotal.toLocaleString()} chunks across{' '}
              {stuckBucketCount.toLocaleString()}{' '}
              {stuckBucketCount === 1 ? 'bucket' : 'buckets'} would be
              stranded.
            </div>
            <div className="leading-snug">
              Update placement policy on each stuck bucket so at least one
              live cluster can accept its chunks, then retry.
            </div>
            {onOpenBulkFix && stuckBucketCount > 0 && (
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="border-amber-500/60 text-amber-800 hover:bg-amber-500/15 dark:text-amber-200"
                onClick={() => onOpenBulkFix(stuckBuckets)}
                data-testid="cd-bulk-fix"
              >
                <Wrench className="mr-1 h-3.5 w-3.5" aria-hidden />
                Fix {stuckBucketCount}{' '}
                {stuckBucketCount === 1 ? 'bucket' : 'buckets'}
              </Button>
            )}
          </div>
        </div>
      ) : (
        <div className="rounded-md border border-emerald-500/40 bg-emerald-500/10 p-2 text-xs text-emerald-800 dark:text-emerald-300">
          All chunks on this cluster have a live target. Submit to start the
          evacuation.
        </div>
      )}
    </div>
  );
}

interface ImpactCounterProps {
  tone: 'ok' | 'stuck';
  label: string;
  value: number;
}

function ImpactCounter({ tone, label, value }: ImpactCounterProps) {
  return (
    <div
      className={cn(
        'rounded-md border p-2 tabular-nums',
        tone === 'ok'
          ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-800 dark:text-emerald-300'
          : 'border-amber-500/40 bg-amber-500/10 text-amber-800 dark:text-amber-300',
      )}
    >
      <div className="text-lg font-semibold leading-none">
        {value.toLocaleString()}
      </div>
      <div className="mt-1 text-[10px] uppercase tracking-wide">{label}</div>
    </div>
  );
}
