// Reconcile-job wrappers for /admin/v1/reconcile (US-006). Wire shape mirrors
// adminapi.ReconcileJSON in internal/adminapi/reconcile.go.
//
// The console mirrors the drain / reshard-progress UX: POST queues a job (202),
// the leader-elected `reconcile` worker drains it out of band, and the GET
// endpoint is polled to watch the pass converge (queued → running → done).
//
// Two passes, discriminated by which params are set:
//   - ORPHAN  (data→meta): cluster + pool (+ optional namespace). Policy is
//     report (default), gc, or restore. Walks the RADOS pool, attributes each
//     chunk to an owner via its back-reference, flags chunks no manifest
//     references. restore (US-002b) rebuilds the manifest row from the
//     back-reference for a genuinely-absent version (plaintext-only).
//   - DANGLING (meta→data): bucket (a bucket NAME). Policy is report (default)
//     or quarantine. Walks every object version's manifest and probes each
//     chunk; a manifest with a missing chunk is dangling.
//
// rebuild-index is intentionally NOT here — it is a destructive last-resort
// CLI-only op (`strata admin rebuild-index`) gated behind shell access; the
// console links to the runbook instead of exposing a one-click rebuild.

export type ReconcileState = 'queued' | 'running' | 'done' | 'error';

export type ReconcilePass = 'orphan' | 'dangling';

// Orphan-pass policies. restore (US-002b) rebuilds the manifest row from the
// back-reference for a genuinely-absent version.
export type OrphanPolicy = 'report' | 'gc' | 'restore';
// Dangling-pass policies.
export type DanglingPolicy = 'report' | 'quarantine';

export interface ReconcileJob {
  ok: boolean;
  id: string;
  cluster?: string;
  pool?: string;
  namespace?: string;
  bucket?: string;
  policy: string;
  state: ReconcileState;
  cursor?: string;
  scanned: number;
  orphans_found: number;
  orphans_gc: number;
  orphans_report: number;
  orphans_restore: number;
  absent_backref: number;
  manifests_scanned: number;
  healthy: number;
  dangling_found: number;
  dangling_quarantine: number;
  dangling_report: number;
  errors: number;
  message?: string;
  started_at?: number;
  updated_at?: number;
}

export interface StartReconcileRequest {
  cluster?: string;
  pool?: string;
  namespace?: string;
  bucket?: string;
  policy?: string;
}

// extractErrorMessage pulls the server's JSON {code,message} so the form can
// render the inline error (e.g. 400 InvalidArgument on a missing pool, 404
// NoSuchBucket on a dangling pass against a missing bucket).
async function extractErrorMessage(resp: Response): Promise<string> {
  try {
    const j = (await resp.json()) as { message?: string; code?: string };
    if (j.message) return j.message;
    if (j.code) return j.code;
  } catch {
    // ignore JSON parse failure
  }
  return `${resp.status} ${resp.statusText}`;
}

// startReconcile queues a reconcile pass and returns the queued job descriptor
// (202). Throws on 4xx/5xx with the server message so the form renders the
// inline error.
export async function startReconcile(
  req: StartReconcileRequest,
): Promise<ReconcileJob> {
  const resp = await fetch('/admin/v1/reconcile', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  });
  if (!resp.ok) {
    throw new Error(`start reconcile: ${await extractErrorMessage(resp)}`);
  }
  return (await resp.json()) as ReconcileJob;
}

// fetchReconcileJob reads a reconcile job by id so the console can poll a pass
// to completion and render the post-run orphan / dangling summary.
export async function fetchReconcileJob(id: string): Promise<ReconcileJob> {
  const resp = await fetch(`/admin/v1/reconcile/${encodeURIComponent(id)}`, {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (!resp.ok) {
    throw new Error(`reconcile status: ${await extractErrorMessage(resp)}`);
  }
  return (await resp.json()) as ReconcileJob;
}
