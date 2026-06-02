import { useEffect, useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import {
  AlertCircle,
  CheckCircle2,
  ExternalLink,
  Loader2,
  Play,
  ShieldAlert,
  Terminal,
} from 'lucide-react';

import {
  fetchReconcileJob,
  startReconcile,
  type ReconcileJob,
  type ReconcilePass,
} from '@/api/client';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
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

// Poll fast while a job is in flight so the operator watches the pass converge
// in near real time — same cadence the reshard panel uses.
const RECONCILE_POLL_MS = 2_000;

// The runbook the rebuild-index card links to. rebuild-index is intentionally
// CLI-ONLY (a destructive last-resort op behind shell access) — the console
// never exposes a one-click rebuild. Kept as a literal so the Playwright spec
// can assert the link target.
export const REBUILD_RUNBOOK_URL =
  'https://danchupin.github.io/strata/operate/metadata-data-reconcile/';

function formatElapsed(sinceSec: number): string {
  if (!Number.isFinite(sinceSec) || sinceSec <= 0) return '0s';
  const sec = Math.floor(sinceSec);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ${sec % 60}s`;
  const hr = Math.floor(min / 60);
  return `${hr}h ${min % 60}m`;
}

export function ReconcilePage() {
  const [pass, setPass] = useState<ReconcilePass>('orphan');
  // Orphan-pass inputs.
  const [cluster, setCluster] = useState('');
  const [pool, setPool] = useState('');
  const [namespace, setNamespace] = useState('');
  // Dangling-pass input.
  const [bucket, setBucket] = useState('');
  // Policy is pass-scoped: orphan → report|gc, dangling → report|quarantine.
  const [policy, setPolicy] = useState('report');

  const [jobID, setJobID] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // wasActive flips true once we observe a queued/running job so a later
  // done/idle observation renders the completion summary, not the steady form.
  const [wasActive, setWasActive] = useState(false);

  // Reset the policy to the pass default whenever the pass flips so an orphan
  // gc policy never leaks into a dangling submit (the API rejects it, but the
  // picker should never show it either).
  useEffect(() => {
    setPolicy('report');
  }, [pass]);

  const q = useQuery<ReconcileJob>({
    queryKey: jobID ? queryKeys.reconcileJob(jobID) : ['reconcile', 'none'],
    queryFn: () => fetchReconcileJob(jobID as string),
    enabled: jobID != null,
    refetchInterval: RECONCILE_POLL_MS,
    placeholderData: keepPreviousData,
    meta: { label: 'reconcile status', silent: true },
  });

  const job = q.data;
  const active = job?.state === 'queued' || job?.state === 'running';

  useEffect(() => {
    if (active) setWasActive(true);
  }, [active]);

  const policyOptions = useMemo(
    () =>
      pass === 'orphan'
        ? [
            { value: 'report', label: 'report — count only (safe default)' },
            { value: 'gc', label: 'gc — enqueue orphan chunks for deletion' },
            {
              value: 'restore',
              label: 'restore — rebuild manifest from back-reference',
            },
          ]
        : [
            { value: 'report', label: 'report — count only (safe default)' },
            {
              value: 'quarantine',
              label: 'quarantine — mark object unreadable',
            },
            {
              value: 'delete',
              label: 'delete — GC chunks + remove broken object',
            },
          ],
    [pass],
  );

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitting) return;
    setError(null);
    setSubmitting(true);
    try {
      const req =
        pass === 'orphan'
          ? {
              cluster: cluster.trim(),
              pool: pool.trim(),
              namespace: namespace.trim() || undefined,
              policy,
            }
          : { bucket: bucket.trim(), policy };
      const queued = await startReconcile(req);
      setJobID(queued.id);
      setWasActive(false);
      void queryClient.invalidateQueries({
        queryKey: queryKeys.reconcileJob(queued.id),
      });
      showToast({
        title: 'Reconcile queued',
        description: `${pass === 'orphan' ? 'Orphan-chunk' : 'Dangling-manifest'} pass running in the background.`,
      });
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed to start reconcile');
    } finally {
      setSubmitting(false);
    }
  }

  const submitDisabled =
    submitting ||
    active ||
    (pass === 'orphan'
      ? cluster.trim() === '' || pool.trim() === ''
      : bucket.trim() === '');

  return (
    <div className="space-y-6" data-testid="reconcile-page">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Reconcile</h1>
        <p className="text-sm text-muted-foreground">
          Repair metadata ↔ data divergence after a restore. An{' '}
          <strong>orphan</strong> pass walks a RADOS pool and flags chunks no
          manifest references; a <strong>dangling</strong> pass walks a bucket's
          manifests and flags chunks the data tier no longer has. Runs as a
          leader-elected background worker — queue it here and watch it converge.
        </p>
      </div>

      <Card data-testid="reconcile-form-card">
        <CardHeader>
          <CardTitle className="text-base">Start a reconcile pass</CardTitle>
          <CardDescription>
            The pass is queued (202) and drained by the{' '}
            <code className="font-mono">reconcile</code> worker — enable it with{' '}
            <code className="font-mono">STRATA_WORKERS=…,reconcile</code> on at
            least one replica.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="reconcile-pass">Pass direction</Label>
              <Select
                value={pass}
                onValueChange={(v) => setPass(v as ReconcilePass)}
              >
                <SelectTrigger id="reconcile-pass" data-testid="reconcile-pass">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="orphan">
                    Orphan chunks (data → meta)
                  </SelectItem>
                  <SelectItem value="dangling">
                    Dangling manifests (meta → data)
                  </SelectItem>
                </SelectContent>
              </Select>
            </div>

            {pass === 'orphan' ? (
              <div className="grid gap-4 sm:grid-cols-3">
                <div className="space-y-1.5">
                  <Label htmlFor="reconcile-cluster">Cluster</Label>
                  <Input
                    id="reconcile-cluster"
                    data-testid="reconcile-cluster"
                    value={cluster}
                    onChange={(e) => setCluster(e.target.value)}
                    placeholder="ceph-a"
                    disabled={submitting || active}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="reconcile-pool">Pool</Label>
                  <Input
                    id="reconcile-pool"
                    data-testid="reconcile-pool"
                    value={pool}
                    onChange={(e) => setPool(e.target.value)}
                    placeholder="strata-data"
                    disabled={submitting || active}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="reconcile-namespace">
                    Namespace (optional)
                  </Label>
                  <Input
                    id="reconcile-namespace"
                    data-testid="reconcile-namespace"
                    value={namespace}
                    onChange={(e) => setNamespace(e.target.value)}
                    placeholder="default"
                    disabled={submitting || active}
                  />
                </div>
              </div>
            ) : (
              <div className="space-y-1.5">
                <Label htmlFor="reconcile-bucket">Bucket</Label>
                <Input
                  id="reconcile-bucket"
                  data-testid="reconcile-bucket"
                  value={bucket}
                  onChange={(e) => setBucket(e.target.value)}
                  placeholder="my-bucket"
                  disabled={submitting || active}
                />
              </div>
            )}

            <div className="space-y-1.5">
              <Label htmlFor="reconcile-policy">Policy</Label>
              <Select value={policy} onValueChange={setPolicy}>
                <SelectTrigger
                  id="reconcile-policy"
                  data-testid="reconcile-policy"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {policyOptions.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                <code className="font-mono">restore</code> (rebuild a manifest
                from back-references) is pending and not offered here.
              </p>
            </div>

            {error && (
              <div
                data-testid="reconcile-error"
                className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive"
              >
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                <div className="text-xs text-destructive/90">{error}</div>
              </div>
            )}

            <Button
              type="submit"
              data-testid="reconcile-submit"
              disabled={submitDisabled}
            >
              {submitting ? (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              ) : (
                <Play className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              )}
              Start reconcile
            </Button>
          </form>
        </CardContent>
      </Card>

      {job && <ReconcileStatus job={job} wasActive={wasActive} />}

      <RebuildIndexCard />
    </div>
  );
}

function ReconcileStatus({
  job,
  wasActive,
}: {
  job: ReconcileJob;
  wasActive: boolean;
}) {
  const active = job.state === 'queued' || job.state === 'running';
  const done = job.state === 'done' || job.state === 'error';
  const isOrphan = !job.bucket;
  const elapsed =
    job.started_at && job.started_at > 0
      ? formatElapsed(Date.now() / 1000 - job.started_at)
      : null;

  return (
    <Card data-testid="reconcile-status-card">
      <CardHeader className="space-y-1">
        <CardTitle className="text-base">
          {isOrphan ? 'Orphan-chunk pass' : 'Dangling-manifest pass'}
        </CardTitle>
        <CardDescription>
          <span className="font-mono" data-testid="reconcile-job-id">
            {job.id}
          </span>{' '}
          · policy <span className="font-mono">{job.policy}</span>
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-center justify-between text-sm">
          <span
            className="inline-flex items-center gap-1.5"
            data-testid="reconcile-state"
          >
            {active && (
              <Loader2
                className="h-3.5 w-3.5 animate-spin text-muted-foreground"
                aria-hidden
              />
            )}
            {job.state === 'done' && (
              <CheckCircle2
                className="h-4 w-4 text-emerald-600 dark:text-emerald-400"
                aria-hidden
              />
            )}
            {job.state === 'error' && (
              <AlertCircle className="h-4 w-4 text-destructive" aria-hidden />
            )}
            <span className="capitalize">{job.state}</span>
          </span>
          {elapsed && (
            <span className="tabular-nums text-muted-foreground">
              {elapsed} elapsed
            </span>
          )}
        </div>

        {active && (
          <div className="h-2 overflow-hidden rounded-full bg-muted">
            <div
              className={cn(
                'h-full bg-primary transition-[width] duration-300',
                job.state === 'running' ? 'w-full animate-pulse' : 'w-1/12',
              )}
            />
          </div>
        )}

        {isOrphan ? (
          <div
            data-testid="reconcile-orphan-counters"
            className="grid grid-cols-2 gap-3 sm:grid-cols-3"
          >
            <Counter label="Chunks scanned" value={job.scanned} />
            <Counter label="Orphans found" value={job.orphans_found} />
            <Counter label="GC'd" value={job.orphans_gc} />
            <Counter label="Restored" value={job.orphans_restore} />
            <Counter label="Reported" value={job.orphans_report} />
            <Counter label="No back-ref" value={job.absent_backref} />
            <Counter label="Errors" value={job.errors} tone="error" />
          </div>
        ) : (
          <div
            data-testid="reconcile-dangling-counters"
            className="grid grid-cols-2 gap-3 sm:grid-cols-3"
          >
            <Counter label="Manifests scanned" value={job.manifests_scanned} />
            <Counter label="Healthy" value={job.healthy} />
            <Counter label="Dangling found" value={job.dangling_found} />
            <Counter label="Quarantined" value={job.dangling_quarantine} />
            <Counter label="Reported" value={job.dangling_report} />
            <Counter label="Deleted" value={job.dangling_delete} />
            <Counter label="Errors" value={job.errors} tone="error" />
          </div>
        )}

        {job.cursor && active && (
          <div
            data-testid="reconcile-cursor"
            className="truncate font-mono text-[11px] text-muted-foreground"
            title={job.cursor}
          >
            cursor: {job.cursor}
          </div>
        )}

        {done && job.message && (
          <div
            data-testid="reconcile-message"
            className="rounded-md border border-border/60 bg-muted/40 p-2 text-xs text-muted-foreground"
          >
            {job.message}
          </div>
        )}

        {wasActive && job.state === 'done' && (
          <div
            data-testid="reconcile-complete"
            className="flex items-center gap-2 rounded-md border border-emerald-500/40 bg-emerald-500/5 p-2 text-sm text-emerald-700 dark:text-emerald-300"
          >
            <CheckCircle2 className="h-4 w-4 shrink-0" aria-hidden />
            Reconcile complete — review the summary above before choosing a
            destructive policy.
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function Counter({
  label,
  value,
  tone,
}: {
  label: string;
  value: number;
  tone?: 'error';
}) {
  return (
    <div className="space-y-0.5">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div
        className={cn(
          'text-lg font-semibold tabular-nums',
          tone === 'error' && value > 0 && 'text-destructive',
        )}
      >
        {value.toLocaleString()}
      </div>
    </div>
  );
}

// RebuildIndexCard documents that rebuild-index is a deliberate CLI-only op
// (US-006 acceptance: the console links to the runbook instead of exposing a
// one-click destructive last-resort rebuild gated behind shell access).
function RebuildIndexCard() {
  return (
    <Card data-testid="rebuild-index-card" className="border-amber-500/40">
      <CardHeader className="space-y-1">
        <CardTitle className="flex items-center gap-2 text-base">
          <ShieldAlert className="h-4 w-4 text-amber-600" aria-hidden />
          Rebuild index — CLI only
        </CardTitle>
        <CardDescription>
          Reconstructing the manifest index from a full data-tier scan is a
          last-resort, irreversible operation. It is intentionally not exposed
          in the console — run it from a shell with{' '}
          <code className="font-mono">strata admin rebuild-index</code>.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2 text-sm text-muted-foreground">
        <p className="inline-flex items-center gap-2 rounded-md bg-muted/50 px-2 py-1 font-mono text-xs">
          <Terminal className="h-3.5 w-3.5 shrink-0" aria-hidden />
          strata admin rebuild-index --dry-run
        </p>
        <a
          href={REBUILD_RUNBOOK_URL}
          target="_blank"
          rel="noreferrer"
          data-testid="rebuild-runbook-link"
          className="inline-flex items-center gap-1 text-foreground underline-offset-2 hover:underline"
        >
          Reconcile &amp; rebuild runbook
          <ExternalLink className="h-3 w-3" aria-hidden />
        </a>
      </CardContent>
    </Card>
  );
}

export default ReconcilePage;
