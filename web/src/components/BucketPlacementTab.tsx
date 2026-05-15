import { useEffect, useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, AlertTriangle, Info, Loader2, RotateCcw } from 'lucide-react';

import {
  deleteBucketPlacement,
  fetchBucketPlacement,
  fetchClusters,
  setBucketPlacement,
  type BucketDetail,
  type ClusterStateEntry,
  type PlacementMode,
} from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
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
import { Skeleton } from '@/components/ui/skeleton';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  bucket: BucketDetail;
}

interface RowState {
  weight: number;
}

function clampWeight(n: number): number {
  if (!Number.isFinite(n)) return 0;
  if (n < 0) return 0;
  if (n > 100) return 100;
  return Math.round(n);
}

// editableClusters excludes 'removed' clusters — the operator can't route
// to a tombstoned cluster anyway. Draining stays editable per AC because
// the rebalance worker moves chunks off them and a future un-drain may
// restore live state.
function editableClusters(all: ClusterStateEntry[]): ClusterStateEntry[] {
  return all.filter((c) => c.state !== 'removed');
}

function buildInitialRows(
  clusters: ClusterStateEntry[],
  policy: Record<string, number> | null,
): Record<string, RowState> {
  const out: Record<string, RowState> = {};
  for (const c of clusters) {
    const w = policy?.[c.id] ?? 0;
    out[c.id] = { weight: clampWeight(w) };
  }
  // Carry policy entries for unknown cluster ids forward too — the server
  // already validated the set at write time; the UI must not silently drop
  // them on edit.
  if (policy) {
    for (const [id, w] of Object.entries(policy)) {
      if (!(id in out)) out[id] = { weight: clampWeight(w) };
    }
  }
  return out;
}

function rowsEqual(
  a: Record<string, RowState>,
  b: Record<string, RowState>,
): boolean {
  const ak = Object.keys(a);
  const bk = Object.keys(b);
  if (ak.length !== bk.length) return false;
  for (const k of ak) {
    if (!(k in b)) return false;
    if (a[k].weight !== b[k].weight) return false;
  }
  return true;
}

function rowsToPlacement(rows: Record<string, RowState>): Record<string, number> {
  const out: Record<string, number> = {};
  for (const [id, r] of Object.entries(rows)) {
    if (r.weight > 0) out[id] = r.weight;
  }
  return out;
}

export function BucketPlacementTab({ bucket }: Props) {
  const clustersQ = useQuery({
    queryKey: queryKeys.clusters,
    queryFn: fetchClusters,
    placeholderData: keepPreviousData,
    meta: { label: 'clusters' },
  });

  const placementQ = useQuery({
    queryKey: queryKeys.buckets.placement(bucket.name),
    queryFn: () => fetchBucketPlacement(bucket.name),
    meta: { silent: true },
  });

  const clusters = useMemo(
    () => editableClusters(clustersQ.data?.clusters ?? []),
    [clustersQ.data],
  );
  const initial = useMemo(
    () => buildInitialRows(clusters, placementQ.data?.placement ?? null),
    [clusters, placementQ.data],
  );
  const initialMode: PlacementMode = placementQ.data?.mode ?? 'weighted';

  const [rows, setRows] = useState<Record<string, RowState>>(initial);
  const [mode, setMode] = useState<PlacementMode>(initialMode);
  const [saving, setSaving] = useState(false);
  const [resetting, setResetting] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [strictConfirmOpen, setStrictConfirmOpen] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);

  useEffect(() => {
    setRows(initial);
    setServerError(null);
  }, [initial]);

  useEffect(() => {
    setMode(initialMode);
  }, [initialMode]);

  const dirty = !rowsEqual(rows, initial) || mode !== initialMode;
  const hasPolicy =
    placementQ.data != null && Object.keys(placementQ.data.placement).length > 0;

  // policyAllDraining: every cluster id with non-zero weight in the saved
  // policy maps to a cluster whose state=draining. The warning is read from
  // the same shared `clusters` query so no extra fetch — and the saved
  // (placementQ.data) shape feeds the check rather than the dirty `rows`
  // editor state, so editing toward a healthy cluster doesn't briefly clear
  // the warning until the operator hits Save.
  const drainingByID = useMemo<Set<string>>(() => {
    const out = new Set<string>();
    for (const c of clustersQ.data?.clusters ?? []) {
      if (c.state === 'draining') out.add(c.id);
    }
    return out;
  }, [clustersQ.data]);
  const policyAllDraining = useMemo<boolean>(() => {
    if (!hasPolicy) return false;
    const ids = Object.entries(placementQ.data?.placement ?? {})
      .filter(([, w]) => (w ?? 0) > 0)
      .map(([id]) => id);
    if (ids.length === 0) return false;
    return ids.every((id) => drainingByID.has(id));
  }, [hasPolicy, placementQ.data, drainingByID]);

  // Validation: at least one weight > 0 AND every weight in [0, 100]
  const allWeights = Object.values(rows).map((r) => r.weight);
  const sumPositive = allWeights.some((w) => w > 0);
  const allInRange = allWeights.every((w) => w >= 0 && w <= 100);
  const saveDisabled = saving || !dirty || !sumPositive || !allInRange;

  // Disable-state tooltip explains which rule blocks the save.
  const saveTooltip = !dirty
    ? 'No changes to save'
    : !sumPositive
      ? 'At least one cluster weight must be greater than 0'
      : !allInRange
        ? 'Weights must be between 0 and 100'
        : '';

  function setWeight(id: string, value: number) {
    setRows((prev) => ({ ...prev, [id]: { weight: clampWeight(value) } }));
  }

  async function handleSave() {
    setSaving(true);
    setServerError(null);
    try {
      const placement = rowsToPlacement(rows);
      await setBucketPlacement(bucket.name, placement, mode);
      await queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.placement(bucket.name),
      });
      showToast({
        title: 'Placement policy updated',
        description: bucket.name,
      });
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setServerError(msg);
      showToast({
        title: 'Failed to update placement',
        description: msg,
        variant: 'destructive',
      });
    } finally {
      setSaving(false);
    }
  }

  async function handleReset() {
    setResetting(true);
    setServerError(null);
    try {
      await deleteBucketPlacement(bucket.name);
      await queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.placement(bucket.name),
      });
      showToast({
        title: 'Placement reset to default routing',
        description: bucket.name,
      });
      setConfirmOpen(false);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setServerError(msg);
      showToast({
        title: 'Failed to reset placement',
        description: msg,
        variant: 'destructive',
      });
    } finally {
      setResetting(false);
    }
  }

  const loading = clustersQ.isPending || placementQ.isPending;
  const loadError =
    !clustersQ.data && clustersQ.error instanceof Error
      ? clustersQ.error.message
      : null;
  const orderedRowIds = useMemo(() => {
    const ids = Object.keys(rows);
    ids.sort((a, b) => a.localeCompare(b));
    return ids;
  }, [rows]);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Placement policy</CardTitle>
        <CardDescription>
          Weights distribute new PUTs across clusters proportionally. A bucket
          without a policy falls back to the gateway default routing. Existing
          chunks remain where they are; the rebalance worker migrates them
          asynchronously after drain or policy changes.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {loading && !clustersQ.data && (
          <div className="space-y-2">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </div>
        )}

        {loadError && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load clusters</div>
              <div className="text-xs text-destructive/80">{loadError}</div>
            </div>
          </div>
        )}

        {!loading && !loadError && clusters.length === 0 && (
          <div className="flex items-start gap-2 rounded-md border bg-muted/30 p-2 text-sm text-muted-foreground">
            <Info className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>No clusters registered. Configure clusters to set placement.</div>
          </div>
        )}

        {!loading && !loadError && !hasPolicy && clusters.length > 0 && (
          <div className="flex items-start gap-2 rounded-md border bg-muted/30 p-2 text-sm text-muted-foreground">
            <Info className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>Default routing (no per-bucket policy).</div>
          </div>
        )}

        {!loading && policyAllDraining && (
          <div
            role="alert"
            data-testid="policy-drain-warning"
            className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-sm text-amber-800 dark:text-amber-300"
          >
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div className="text-xs">
              <div className="font-medium">
                All clusters in this policy are draining
              </div>
              New PUTs will be refused (strict mode) or fall back to the class
              default (default mode). Update the policy before traffic resumes.
            </div>
          </div>
        )}

        {!loading && clusters.length > 0 && (
          <div
            className="flex items-start justify-between gap-3 rounded-md border p-3"
            title="When ON, this bucket will refuse PUTs and block drain if all clusters in its Placement policy are draining. When OFF (default), it falls back to cluster default routing weights. Turn ON for compliance-sensitive buckets (data-sovereignty, replication design)."
          >
            <div className="flex-1">
              <Label htmlFor="placement-mode-switch" className="text-sm font-medium">
                Strict placement
              </Label>
              <div className="mt-1 text-xs text-muted-foreground">
                Refuse PUTs and block drain when every cluster in this
                policy is draining (compliance / data-sovereignty pin).
                Default: off — falls back to cluster default routing
                weights.
              </div>
            </div>
            <Switch
              id="placement-mode-switch"
              checked={mode === 'strict'}
              onCheckedChange={(next) => {
                if (next) {
                  // Opt-in is the destructive direction (may block drain) —
                  // require explicit confirmation. Relaxing back to
                  // weighted is one-click per AC.
                  setStrictConfirmOpen(true);
                } else {
                  setMode('weighted');
                }
              }}
              disabled={saving || resetting}
              aria-label="Strict placement mode"
            />
          </div>
        )}

        {!loading && clusters.length > 0 && (
          <div className="space-y-2">
            {orderedRowIds.map((id) => {
              const cluster = clusters.find((c) => c.id === id);
              const weight = rows[id]?.weight ?? 0;
              const draining = cluster?.state === 'draining';
              return (
                <div
                  key={id}
                  className="grid grid-cols-[1fr,2fr,5rem] items-center gap-3 rounded-md border p-3"
                >
                  <div className="flex items-center gap-2">
                    <Label className="font-mono text-sm" htmlFor={`pl-slider-${id}`}>
                      {id}
                    </Label>
                    {draining && (
                      <Badge variant="warning" className="text-[10px]">
                        draining
                      </Badge>
                    )}
                    {!cluster && (
                      <Badge variant="secondary" className="text-[10px]">
                        not registered
                      </Badge>
                    )}
                  </div>
                  <input
                    id={`pl-slider-${id}`}
                    type="range"
                    min={0}
                    max={100}
                    step={1}
                    value={weight}
                    onChange={(e) => setWeight(id, Number(e.target.value))}
                    disabled={saving || resetting}
                    aria-label={`weight for ${id}`}
                    className="h-2 w-full cursor-pointer appearance-none rounded-full bg-muted accent-primary"
                  />
                  <Input
                    type="number"
                    min={0}
                    max={100}
                    step={1}
                    value={weight}
                    onChange={(e) => {
                      const v = e.target.value;
                      if (v === '') {
                        setWeight(id, 0);
                        return;
                      }
                      const n = Number(v);
                      if (Number.isFinite(n)) setWeight(id, n);
                    }}
                    disabled={saving || resetting}
                    aria-label={`weight input for ${id}`}
                    className="h-8 text-right tabular-nums"
                  />
                </div>
              );
            })}
          </div>
        )}

        {serverError && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div className="text-xs">{serverError}</div>
          </div>
        )}
      </CardContent>
      <CardFooter className="justify-end gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => setConfirmOpen(true)}
          disabled={!hasPolicy || resetting || saving}
        >
          <RotateCcw className="mr-1.5 h-3.5 w-3.5" aria-hidden />
          Reset to default
        </Button>
        <span title={saveTooltip}>
          <Button
            type="button"
            size="sm"
            onClick={handleSave}
            disabled={saveDisabled}
          >
            {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
            Save placement
          </Button>
        </span>
      </CardFooter>

      <Dialog open={confirmOpen} onOpenChange={(v) => !resetting && setConfirmOpen(v)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Reset to default routing?</DialogTitle>
            <DialogDescription>
              Removes the per-bucket placement policy for{' '}
              <code className="font-mono">{bucket.name}</code>. New PUTs route
              via the gateway default. Existing chunks are unaffected.
            </DialogDescription>
          </DialogHeader>
          <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-sm text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div className="text-xs">
              Reversible — set a new policy any time via the slider editor.
            </div>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setConfirmOpen(false)}
              disabled={resetting}
            >
              Cancel
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={handleReset}
              disabled={resetting}
            >
              {resetting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Reset
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={strictConfirmOpen} onOpenChange={setStrictConfirmOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Enable strict placement?</DialogTitle>
            <DialogDescription>
              Strict placement may block drain workflows if this bucket&apos;s
              clusters become unavailable. Continue?
            </DialogDescription>
          </DialogHeader>
          <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-sm text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div className="text-xs">
              PUTs will return 503 when every cluster in this policy is
              draining; drain workflows refuse to fire until the policy is
              edited. Choose strict only for compliance / data-sovereignty
              buckets where falling back to cluster default routing is
              unacceptable.
            </div>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setStrictConfirmOpen(false)}
            >
              Cancel
            </Button>
            <Button
              type="button"
              onClick={() => {
                setMode('strict');
                setStrictConfirmOpen(false);
              }}
            >
              Enable strict
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}
