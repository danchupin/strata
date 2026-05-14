import { useEffect, useState } from 'react';
import { AlertCircle, AlertTriangle, Loader2 } from 'lucide-react';

import { activateCluster } from '@/api/client';
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
import { Slider } from '@/components/ui/slider';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  clusterID: string;
}

const DEFAULT_WEIGHT = 10;

function clampWeight(n: number): number {
  if (!Number.isFinite(n)) return 0;
  if (n < 0) return 0;
  if (n > 100) return 100;
  return Math.round(n);
}

// ActivateClusterModal arms an initial-weight slider for a pending cluster
// and submits POST /admin/v1/clusters/{id}/activate {weight} on confirm.
// Mirrors the typed-confirm precedent from ConfirmDrainModal — Submit stays
// disabled until the operator types the exact cluster id (case-sensitive).
export function ActivateClusterModal({ open, onOpenChange, clusterID }: Props) {
  const [weight, setWeight] = useState<number>(DEFAULT_WEIGHT);
  const [weightInput, setWeightInput] = useState<string>(String(DEFAULT_WEIGHT));
  const [typed, setTyped] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setWeight(DEFAULT_WEIGHT);
      setWeightInput(String(DEFAULT_WEIGHT));
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

  function handleSliderChange(e: React.ChangeEvent<HTMLInputElement>) {
    const v = clampWeight(Number(e.target.value));
    setWeight(v);
    setWeightInput(String(v));
  }

  function handleInputChange(e: React.ChangeEvent<HTMLInputElement>) {
    const raw = e.target.value;
    // Block non-numeric (allow empty so the field can be cleared mid-edit).
    if (raw !== '' && !/^\d+$/.test(raw)) return;
    setWeightInput(raw);
    if (raw === '') return;
    setWeight(clampWeight(Number(raw)));
  }

  function handleInputBlur() {
    if (weightInput === '') {
      setWeight(0);
      setWeightInput('0');
      return;
    }
    const v = clampWeight(Number(weightInput));
    setWeight(v);
    setWeightInput(String(v));
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    setServerError(null);
    setSubmitting(true);
    try {
      await activateCluster(clusterID, weight);
      showToast({
        title: `Cluster ${clusterID} activated`,
        description:
          weight === 0
            ? 'State flipped to live; weight=0 means it won’t receive default-routed writes yet.'
            : `Routing share starts at weight=${weight}. Adjust later via the cluster card slider.`,
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
          <DialogTitle>Activate cluster</DialogTitle>
          <DialogDescription>
            Bring this cluster into the default-routing rotation. New PUTs on
            buckets without an explicit Placement policy will start landing here
            at the chosen share.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="rounded-md border bg-muted/30 p-2 text-xs">
            <div className="text-muted-foreground">Cluster id</div>
            <div
              className="font-mono text-sm"
              data-testid="ac-cluster-id"
              title={clusterID}
            >
              {clusterID}
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="ac-weight-slider">Initial weight</Label>
            <div className="flex items-center gap-3">
              <Slider
                id="ac-weight-slider"
                min={0}
                max={100}
                step={1}
                value={weight}
                onChange={handleSliderChange}
                disabled={submitting}
                data-testid="ac-weight-slider"
                aria-label="Initial weight"
              />
              <Input
                type="number"
                min={0}
                max={100}
                step={1}
                value={weightInput}
                onChange={handleInputChange}
                onBlur={handleInputBlur}
                disabled={submitting}
                className="h-9 w-20 tabular-nums"
                data-testid="ac-weight-input"
                aria-label="Initial weight numeric"
              />
            </div>
            <p className="text-xs leading-snug text-muted-foreground">
              Activating this cluster will start routing a share of new bucket
              PUTs without explicit Placement policy. Initial share = weight ×
              100% / total-live-weights. Adjust later via the cluster card
              slider.
            </p>
          </div>

          <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-sm text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Confirm cluster id</div>
              <div className="text-xs">
                Type <code className="font-mono">{clusterID}</code> exactly to
                arm the Activate button.
              </div>
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="ac-confirm">Cluster id</Label>
            <Input
              id="ac-confirm"
              autoFocus
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              placeholder={clusterID}
              disabled={submitting}
              autoComplete="off"
              autoCorrect="off"
              spellCheck={false}
              data-testid="ac-confirm-input"
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
              variant="default"
              disabled={submitDisabled}
              data-testid="ac-submit"
            >
              {submitting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Activate (weight {weight})
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
