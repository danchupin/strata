import { useEffect, useMemo, useState } from 'react';
import {
  AlertCircle,
  AlertTriangle,
  CheckCircle2,
  Loader2,
} from 'lucide-react';

import {
  normalizePlacementMode,
  setBucketPlacement,
  type BucketImpactEntry,
  type PlacementMode,
  type SuggestedPolicy,
} from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  clusterID: string;
  // stuck is the slice of /drain-impact `by_bucket` rows where category
  // != 'migratable' — the parent (ConfirmDrainModal) computes this from
  // its cached impact response and hands it down so the dialog can avoid
  // a second fetch. The dialog internally filters to placement_mode ===
  // 'strict' (US-005 effective-placement) because weighted stuck buckets
  // auto-resolve via cluster.weights and never reach this surface.
  stuck: BucketImpactEntry[];
}

// strictOnly filters the stuck list to compliance-locked buckets (mode =
// strict). Weighted stuck buckets auto-resolve via cluster.weights post
// US-003 EffectivePolicy, so they never appear in BulkPlacementFixDialog.
export function strictOnly(buckets: BucketImpactEntry[]): BucketImpactEntry[] {
  return buckets.filter((b) => normalizePlacementMode(b.placement_mode) === 'strict');
}

interface ApplyOutcome {
  ok: number;
  failures: Array<{ bucket: string; error: string }>;
}

// uniformOptions returns the set of suggested-policy labels that exist
// on EVERY selected bucket. Operators pick from this intersection so
// "Apply uniform to all selected" lands a coherent label across the
// batch — each bucket still receives its own policy bytes (e.g. the
// "Add all live clusters (uniform)" label drops draining keys in the
// payload, which differ per bucket if Placement maps diverged).
export function uniformOptions(
  buckets: BucketImpactEntry[],
  selected: Record<string, boolean>,
): string[] {
  const active = buckets.filter((b) => selected[b.name] !== false);
  if (active.length === 0) return [];
  const labelSets = active.map(
    (b) => new Set((b.suggested_policies ?? []).map((s) => s.label)),
  );
  const first = labelSets[0];
  const intersection: string[] = [];
  for (const label of first) {
    if (labelSets.every((s) => s.has(label))) intersection.push(label);
  }
  return intersection;
}

// resolveModeOverride decides which `mode` value to send to
// /admin/v1/buckets/{name}/placement alongside the chosen policy. The
// server stamps placement_mode_override on strict-stuck suggestions —
// "weighted" for the Flip shortcut, "strict" for per-cluster
// replacements. Empty/absent values default to "strict" because the
// dialog is filtered to compliance-locked buckets and "leave mode
// unchanged" would silently preserve the current strict flag — which
// matches the operator's intent for replacement suggestions, but
// explicit is clearer and audit-friendly.
export function resolveModeOverride(choice: SuggestedPolicy): PlacementMode {
  return choice.placement_mode_override === 'weighted' ? 'weighted' : 'strict';
}

// resolvePolicy returns the {clusterID: weight} body to PUT for `bucket`
// under the current dialog state. When `applyUniform` is on, the bucket
// gets the suggestion whose label matches `uniformLabel`. Otherwise it
// gets the per-bucket pick at `perBucketIdx[bucket.name]`. Falls back to
// the first suggested_policies entry if the requested label is missing
// (defensive — should not happen because uniformOptions intersects).
export function resolvePolicy(
  bucket: BucketImpactEntry,
  perBucketIdx: Record<string, number>,
  applyUniform: boolean,
  uniformLabel: string,
): SuggestedPolicy | null {
  const suggestions = bucket.suggested_policies ?? [];
  if (suggestions.length === 0) return null;
  if (applyUniform) {
    const match = suggestions.find((s) => s.label === uniformLabel);
    if (match) return match;
    return suggestions[0];
  }
  const idx = perBucketIdx[bucket.name] ?? 0;
  return suggestions[idx] ?? suggestions[0];
}

function summarizePolicy(p: Record<string, number> | null): string {
  if (!p) return 'no policy';
  const entries = Object.entries(p);
  if (entries.length === 0) return 'no policy';
  const nonZero = entries.filter(([, w]) => w > 0);
  const draining = entries.filter(([, w]) => w === 0);
  const head = nonZero.map(([id, w]) => `${id}:${w}`).join(', ');
  if (draining.length === 0) return head || 'no live target';
  return head ? `${head} (${draining.length} draining @0)` : 'no live target';
}

function categoryBadgeClass(cat: string): string {
  switch (cat) {
    case 'stuck_single_policy':
      return 'border-amber-500/40 bg-amber-500/15 text-amber-800 dark:text-amber-300';
    case 'stuck_no_policy':
      return 'border-amber-500/40 bg-amber-500/15 text-amber-800 dark:text-amber-300';
    case 'migratable':
      return 'border-emerald-500/40 bg-emerald-500/15 text-emerald-800 dark:text-emerald-300';
    default:
      return 'border-border bg-muted text-muted-foreground';
  }
}

function categoryLabel(cat: string): string {
  switch (cat) {
    case 'stuck_single_policy':
      return 'stuck single-policy';
    case 'stuck_no_policy':
      return 'stuck no-policy';
    case 'migratable':
      return 'migratable';
    default:
      return cat;
  }
}

// BulkPlacementFixDialog applies suggested placement policies to many
// affected buckets in one workflow so an operator never has to click
// through to BucketDetail per bucket before retrying an evacuate drain.
// Opens FROM ConfirmDrainModal's "Fix N buckets" CTA — see US-004 wiring.
export function BulkPlacementFixDialog({
  open,
  onOpenChange,
  clusterID,
  stuck,
}: Props) {
  // strictStuck is the filtered slice — only compliance-locked rows reach
  // the dialog. Weighted stuck buckets auto-resolve via cluster.weights
  // and are silently dropped here (operator has nothing to do with them).
  const strictStuck = useMemo(() => strictOnly(stuck), [stuck]);
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [perBucketIdx, setPerBucketIdx] = useState<Record<string, number>>({});
  const [applyUniform, setApplyUniform] = useState(false);
  const [uniformLabel, setUniformLabel] = useState<string>('');
  const [applying, setApplying] = useState(false);
  const [doneCount, setDoneCount] = useState(0);

  useEffect(() => {
    if (!open) return;
    const sel: Record<string, boolean> = {};
    const picks: Record<string, number> = {};
    for (const b of strictStuck) {
      sel[b.name] = true;
      picks[b.name] = 0;
    }
    setSelected(sel);
    setPerBucketIdx(picks);
    setApplyUniform(false);
    setUniformLabel('');
    setApplying(false);
    setDoneCount(0);
  }, [open, strictStuck]);

  const selectedCount = useMemo(
    () => strictStuck.filter((b) => selected[b.name] !== false).length,
    [strictStuck, selected],
  );

  const uniformLabels = useMemo(
    () => uniformOptions(strictStuck, selected),
    [strictStuck, selected],
  );

  // Clear an out-of-range uniform pick when the selection shrinks below
  // the previously chosen label's footprint (e.g. operator unchecks the
  // last bucket carrying "Replace draining with cephb").
  useEffect(() => {
    if (!applyUniform) return;
    if (uniformLabel && !uniformLabels.includes(uniformLabel)) {
      setUniformLabel(uniformLabels[0] ?? '');
    } else if (!uniformLabel && uniformLabels.length > 0) {
      setUniformLabel(uniformLabels[0]);
    }
  }, [applyUniform, uniformLabel, uniformLabels]);

  function handleClose(next: boolean) {
    if (applying) return;
    onOpenChange(next);
  }

  function toggleAll(nextChecked: boolean) {
    const next: Record<string, boolean> = {};
    for (const b of strictStuck) next[b.name] = nextChecked;
    setSelected(next);
  }

  async function handleApply() {
    const targets = strictStuck.filter((b) => selected[b.name] !== false);
    if (targets.length === 0) return;
    setApplying(true);
    setDoneCount(0);
    const outcome: ApplyOutcome = { ok: 0, failures: [] };
    for (const b of targets) {
      const choice = resolvePolicy(b, perBucketIdx, applyUniform, uniformLabel);
      if (!choice) {
        outcome.failures.push({
          bucket: b.name,
          error: 'no suggested policy available',
        });
        setDoneCount((n) => n + 1);
        continue;
      }
      // mode override: the server stamps placement_mode_override on
      // strict-stuck suggestions — "weighted" for the Flip shortcut,
      // "strict" for per-cluster replacements that preserve the
      // compliance pin. When absent (defensive), default to "strict"
      // because the bucket is currently compliance-locked.
      const overrideMode = resolveModeOverride(choice);
      try {
        await setBucketPlacement(b.name, choice.policy, overrideMode);
        outcome.ok += 1;
      } catch (err) {
        outcome.failures.push({
          bucket: b.name,
          error: err instanceof Error ? err.message : String(err),
        });
      } finally {
        setDoneCount((n) => n + 1);
      }
    }
    setApplying(false);
    if (outcome.failures.length === 0) {
      showToast({
        title: `Updated placement on ${outcome.ok} ${
          outcome.ok === 1 ? 'bucket' : 'buckets'
        }`,
        description: 'Retry drain — categorized impact counters refreshing.',
      });
    } else if (outcome.ok > 0) {
      showToast({
        title: `Updated ${outcome.ok} of ${targets.length} buckets`,
        description: `Failures: ${outcome.failures
          .map((f) => `${f.bucket}: ${f.error}`)
          .join('; ')}`,
        variant: 'destructive',
      });
    } else {
      showToast({
        title: 'No buckets updated',
        description: outcome.failures
          .map((f) => `${f.bucket}: ${f.error}`)
          .join('; '),
        variant: 'destructive',
      });
    }
    // Always refresh impact + placement caches; even failures may have
    // partially altered server state (e.g. validation rejecting only
    // some buckets).
    void queryClient.invalidateQueries({
      queryKey: queryKeys.clusterDrainImpact(clusterID),
    });
    for (const b of targets) {
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.placement(b.name),
      });
    }
    // Close on any forward progress — operator retries drain from the
    // refreshed ConfirmDrainModal which refetches /drain-impact.
    if (outcome.ok > 0 || outcome.failures.length === 0) {
      onOpenChange(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleClose}>
      <DialogContent className="max-w-3xl">
        <DialogHeader>
          <DialogTitle>Fix compliance-locked buckets</DialogTitle>
          <DialogDescription>
            These buckets are pinned to strict placement and reference only
            draining clusters. Pick a remediation per bucket: flip to
            weighted (auto-fallback to cluster weights), replace the cluster
            (keeps strict), or keep stuck (leave for later). Weighted stuck
            buckets are not shown — they auto-resolve via cluster weights.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-wrap items-center justify-between gap-2 border-y py-2 text-xs">
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => toggleAll(true)}
              disabled={applying}
              data-testid="bpf-select-all"
            >
              Select all
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => toggleAll(false)}
              disabled={applying}
              data-testid="bpf-deselect-all"
            >
              Deselect all
            </Button>
            <span className="text-muted-foreground">
              {selectedCount.toLocaleString()} of{' '}
              {strictStuck.length.toLocaleString()} selected
            </span>
          </div>
          <label className="flex items-center gap-2">
            <input
              type="checkbox"
              checked={applyUniform}
              onChange={(e) => setApplyUniform(e.target.checked)}
              disabled={applying || uniformLabels.length === 0}
              className="h-3.5 w-3.5"
              data-testid="bpf-uniform-toggle"
            />
            <span
              className={cn(
                'text-xs',
                uniformLabels.length === 0
                  ? 'text-muted-foreground/60'
                  : 'text-foreground',
              )}
              title={
                uniformLabels.length === 0
                  ? 'No suggested policy is common to every selected bucket.'
                  : undefined
              }
            >
              Apply uniform to all selected
            </span>
            {applyUniform && uniformLabels.length > 0 && (
              <Select
                value={uniformLabel}
                onValueChange={setUniformLabel}
                disabled={applying}
              >
                <SelectTrigger
                  className="h-7 w-[260px] text-xs"
                  data-testid="bpf-uniform-select"
                >
                  <SelectValue placeholder="Choose suggested policy" />
                </SelectTrigger>
                <SelectContent>
                  {uniformLabels.map((label) => (
                    <SelectItem key={label} value={label}>
                      {label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          </label>
        </div>

        <div
          className="max-h-[420px] overflow-y-auto rounded-md border"
          data-testid="bpf-bucket-table"
        >
          <table className="w-full text-xs">
            <thead className="sticky top-0 z-10 bg-muted/60">
              <tr className="text-left">
                <th className="w-8 px-2 py-2"></th>
                <th className="px-2 py-2 font-medium">Bucket</th>
                <th className="px-2 py-2 font-medium">Current</th>
                <th className="px-2 py-2 font-medium">Category</th>
                <th className="px-2 py-2 font-medium">Apply policy</th>
              </tr>
            </thead>
            <tbody>
              {strictStuck.length === 0 && (
                <tr data-testid="bpf-empty">
                  <td
                    colSpan={5}
                    className="px-3 py-6 text-center text-xs text-muted-foreground"
                  >
                    No compliance-locked buckets to fix. Weighted stuck
                    buckets auto-resolve via cluster weights.
                  </td>
                </tr>
              )}
              {strictStuck.map((b) => {
                const isChecked = selected[b.name] !== false;
                const suggestions = b.suggested_policies ?? [];
                const idx = perBucketIdx[b.name] ?? 0;
                const overrideLabel =
                  applyUniform && uniformLabel ? uniformLabel : null;
                const effective = resolvePolicy(
                  b,
                  perBucketIdx,
                  applyUniform,
                  uniformLabel,
                );
                return (
                  <tr
                    key={b.name}
                    className={cn(
                      'border-t align-top',
                      !isChecked && 'opacity-50',
                    )}
                    data-testid={`bpf-row-${b.name}`}
                  >
                    <td className="px-2 py-2">
                      <input
                        type="checkbox"
                        checked={isChecked}
                        onChange={(e) =>
                          setSelected((prev) => ({
                            ...prev,
                            [b.name]: e.target.checked,
                          }))
                        }
                        disabled={applying}
                        className="h-3.5 w-3.5"
                        aria-label={`Select ${b.name}`}
                      />
                    </td>
                    <td className="px-2 py-2">
                      <div className="font-mono font-medium text-foreground">
                        {b.name}
                      </div>
                      <div className="text-[10px] text-muted-foreground tabular-nums">
                        {b.chunk_count.toLocaleString()} chunks
                      </div>
                    </td>
                    <td className="px-2 py-2 font-mono text-[11px] text-muted-foreground">
                      {summarizePolicy(b.current_policy)}
                    </td>
                    <td className="px-2 py-2">
                      <Badge
                        variant="outline"
                        className={cn(
                          'text-[10px]',
                          categoryBadgeClass(b.category),
                        )}
                      >
                        {categoryLabel(b.category)}
                      </Badge>
                    </td>
                    <td className="px-2 py-2">
                      {suggestions.length === 0 ? (
                        <span className="text-xs text-destructive">
                          No suggestion available
                        </span>
                      ) : (
                        <Select
                          value={
                            overrideLabel
                              ? suggestions.some(
                                  (s) => s.label === overrideLabel,
                                )
                                ? overrideLabel
                                : String(idx)
                              : String(idx)
                          }
                          onValueChange={(v) =>
                            setPerBucketIdx((prev) => ({
                              ...prev,
                              [b.name]: Number(v),
                            }))
                          }
                          disabled={applying || !isChecked || applyUniform}
                        >
                          <SelectTrigger
                            className="h-7 w-full text-xs"
                            data-testid={`bpf-pick-${b.name}`}
                          >
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            {suggestions.map((s, i) => (
                              <SelectItem key={s.label} value={String(i)}>
                                {s.label}
                              </SelectItem>
                            ))}
                            {overrideLabel &&
                              !suggestions.some(
                                (s) => s.label === overrideLabel,
                              ) && (
                                <SelectItem value={overrideLabel} disabled>
                                  {overrideLabel} (n/a)
                                </SelectItem>
                              )}
                          </SelectContent>
                        </Select>
                      )}
                      {isChecked && effective && (
                        <div className="mt-1 font-mono text-[10px] text-muted-foreground">
                          → {summarizePolicy(effective.policy)}
                        </div>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>

        {applying && (
          <div
            className="flex items-center gap-2 rounded-md border bg-muted/40 p-2 text-xs text-muted-foreground"
            data-testid="bpf-progress"
          >
            <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
            Applying {doneCount} / {selectedCount}…
          </div>
        )}
        {!applying && selectedCount === 0 && (
          <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-800 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            Select at least one bucket to apply.
          </div>
        )}
        {!applying && applyUniform && uniformLabels.length === 0 && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-xs text-destructive">
            <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            No suggested policy is shared by every selected bucket. Uncheck
            the toggle and pick per-bucket, or narrow the selection.
          </div>
        )}

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={() => handleClose(false)}
            disabled={applying}
          >
            Cancel
          </Button>
          <Button
            type="button"
            onClick={handleApply}
            disabled={
              applying ||
              selectedCount === 0 ||
              (applyUniform && uniformLabels.length === 0)
            }
            data-testid="bpf-apply"
          >
            {applying ? (
              <>
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
                Applying…
              </>
            ) : (
              <>
                <CheckCircle2 className="mr-2 h-3.5 w-3.5" aria-hidden />
                Fix {selectedCount} compliance-locked{' '}
                {selectedCount === 1 ? 'bucket' : 'buckets'}
              </>
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
