import { useEffect, useRef, useState } from 'react';

import { updateClusterWeight } from '@/api/client';
import { Input } from '@/components/ui/input';
import { Slider } from '@/components/ui/slider';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

interface Props {
  clusterID: string;
  weight: number;
}

const DEBOUNCE_MS = 500;

function clampWeight(n: number): number {
  if (!Number.isFinite(n)) return 0;
  if (n < 0) return 0;
  if (n > 100) return 100;
  return Math.round(n);
}

// LiveClusterWeightSlider is the inline weight control on live cluster
// cards (US-004 cluster-weights). Optimistic UI: slider + numeric input
// update immediately; rapid drags coalesce via a single 500ms timer to
// one PUT /admin/v1/clusters/{id}/weight. On 4xx we revert to the last
// server-accepted value and surface the error toast; 409 surfaces the
// race-against-drain message verbatim.
export function LiveClusterWeightSlider({ clusterID, weight }: Props) {
  const [pendingWeight, setPendingWeight] = useState<number>(weight);
  const [weightInput, setWeightInput] = useState<string>(String(weight));
  const savedRef = useRef<number>(weight);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const draggingRef = useRef<boolean>(false);

  // Adopt upstream weight refreshes from the poll when the operator
  // isn't actively dragging (timer pending) — otherwise the user's
  // in-flight value would be clobbered by stale poll data.
  useEffect(() => {
    if (timerRef.current == null && !draggingRef.current) {
      savedRef.current = weight;
      setPendingWeight(weight);
      setWeightInput(String(weight));
    }
  }, [weight]);

  useEffect(() => {
    return () => {
      if (timerRef.current != null) clearTimeout(timerRef.current);
    };
  }, []);

  function flush(next: number) {
    if (timerRef.current != null) clearTimeout(timerRef.current);
    draggingRef.current = true;
    timerRef.current = setTimeout(async () => {
      timerRef.current = null;
      draggingRef.current = false;
      try {
        await updateClusterWeight(clusterID, next);
        savedRef.current = next;
        void queryClient.invalidateQueries({ queryKey: queryKeys.clusters });
      } catch (err) {
        const status = (err as Error & { status?: number }).status;
        setPendingWeight(savedRef.current);
        setWeightInput(String(savedRef.current));
        if (status === 409) {
          showToast({
            title: 'Cannot update weight: cluster is no longer in live state',
            description:
              'Refreshing cluster state. Re-open the card to see the new state.',
            variant: 'destructive',
          });
          void queryClient.invalidateQueries({ queryKey: queryKeys.clusters });
        } else {
          showToast({
            title: `Failed to update weight for ${clusterID}`,
            description: err instanceof Error ? err.message : String(err),
            variant: 'destructive',
          });
        }
      }
    }, DEBOUNCE_MS);
  }

  function handleSliderChange(e: React.ChangeEvent<HTMLInputElement>) {
    const v = clampWeight(Number(e.target.value));
    setPendingWeight(v);
    setWeightInput(String(v));
    flush(v);
  }

  function handleInputChange(e: React.ChangeEvent<HTMLInputElement>) {
    const raw = e.target.value;
    if (raw !== '' && !/^\d+$/.test(raw)) return;
    setWeightInput(raw);
    if (raw === '') return;
    const v = clampWeight(Number(raw));
    setPendingWeight(v);
    flush(v);
  }

  function handleInputBlur() {
    if (weightInput === '') {
      setPendingWeight(0);
      setWeightInput('0');
      flush(0);
      return;
    }
    const v = clampWeight(Number(weightInput));
    setPendingWeight(v);
    setWeightInput(String(v));
  }

  return (
    <div className="space-y-1">
      <div className="flex items-center gap-2">
        <Slider
          min={0}
          max={100}
          step={1}
          value={pendingWeight}
          onChange={handleSliderChange}
          title="Share in default uniform routing for buckets without explicit Placement policy. weight=0 means no default-routed writes; reads + explicit policies still work."
          data-testid="cluster-card-weight-slider"
          aria-label={`Weight for ${clusterID}`}
        />
        <Input
          type="number"
          min={0}
          max={100}
          step={1}
          value={weightInput}
          onChange={handleInputChange}
          onBlur={handleInputBlur}
          className="h-8 w-16 tabular-nums"
          data-testid="cluster-card-weight-input"
          aria-label={`Weight numeric for ${clusterID}`}
        />
      </div>
      {pendingWeight === 0 && (
        <span
          className="inline-flex rounded-md border bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground"
          data-testid="cluster-card-weight-zero-chip"
        >
          weight=0 — no default-routed writes
        </span>
      )}
    </div>
  );
}
