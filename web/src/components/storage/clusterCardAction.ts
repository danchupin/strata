// clusterCardAction maps the per-cluster (state, chunks, deregister_ready,
// not_ready_reasons) tuple to the action button(s) rendered on the
// ClustersSubsection card. Pure helper — no React, no hooks — so the
// State Truth Table (US-007 drain-cleanup, prd.md table at line 31) can
// be exercised in a node-only unit test without rendering JSX.
//
// The truth table:
//
// | state              | chunks | deregister_ready | not_ready_reasons | kind                          |
// |--------------------|--------|------------------|-------------------|-------------------------------|
// | pending            | —      | —                | —                 | activate                      |
// | live               | —      | —                | —                 | drain                         |
// | draining_readonly  | —      | —                | —                 | none (Upgrade+Undrain live in DrainProgressBar) |
// | evacuating         | >0     | false            | —                 | undrain-confirm-evacuation    |
// | evacuating         | 0      | false            | gc_queue_pending  | undrain-disabled-gc           |
// | evacuating         | 0      | true             | —                 | cancel-deregister-prep        |
// | removed            | —      | —                | —                 | drain-disabled                |
//
// `none` means the card body renders no bottom-right action button;
// DrainProgressBar owns Upgrade + Undrain in the readonly-drain shape.
// `drain-disabled` keeps the legacy disabled-Drain affordance for
// `removed` rows that the operator hasn't yet purged from env.

export type ClusterCardActionKind =
  | 'activate'
  | 'drain'
  | 'drain-disabled'
  | 'undrain-confirm-evacuation'
  | 'undrain-disabled-gc'
  | 'cancel-deregister-prep'
  | 'none';

export interface ClusterCardActionInput {
  state: string;
  // chunks is null while drain-progress is still loading. Treat null as
  // "unknown" — the helper falls through to `undrain-confirm-evacuation`
  // for evacuating clusters because that is the safe default (the
  // confirm modal warns about no rollback; the disabled-GC + cancel-
  // prep variants are only reachable once we know chunks == 0).
  chunks: number | null;
  deregisterReady: boolean;
  notReadyReasons: string[];
}

export function clusterCardAction(
  input: ClusterCardActionInput,
): ClusterCardActionKind {
  const s = (input.state ?? '').toLowerCase();
  switch (s) {
    case 'pending':
      return 'activate';
    case 'live':
      return 'drain';
    case 'draining_readonly':
      return 'none';
    case 'evacuating': {
      const chunks = input.chunks;
      if (chunks == null || chunks > 0) {
        return 'undrain-confirm-evacuation';
      }
      // chunks === 0
      if (input.deregisterReady) {
        return 'cancel-deregister-prep';
      }
      return 'undrain-disabled-gc';
    }
    case 'removed':
      return 'drain-disabled';
    default:
      return 'drain';
  }
}

// undrainDisabledTooltip returns the title= text rendered on the
// disabled Undrain button for the (evacuating + chunks=0 +
// !deregister_ready) cell. The text quotes the unmet probe(s) so the
// operator can route to the GC queue (or the open-multipart code path)
// without reading drain-progress JSON.
export function undrainDisabledTooltip(notReadyReasons: string[]): string {
  if (notReadyReasons.length === 0) {
    return 'Cannot undrain while GC queue is processing.';
  }
  const reasons = notReadyReasons.join(', ');
  return `Cannot undrain while safety probes are pending: ${reasons}.`;
}
