import { Suspense, lazy, useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, CheckCircle2, Loader2 } from 'lucide-react';

import {
  deleteBucketPolicy,
  dryRunBucketPolicy,
  fetchBucketPolicyText,
  setBucketPolicy,
  type AdminApiError,
  type BucketDetail,
} from '@/api/client';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
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
import schemaJson from '@/schemas/iam-policy.json';

// Lazy-loaded — reuses the Monaco chunk shared with Lifecycle / CORS so the
// per-page bundle stays under the 500 KiB gzipped budget.
const MonacoJsonEditor = lazy(() => import('./MonacoJsonEditor'));

interface Props {
  bucket: BucketDetail;
}

type DryRunState =
  | { kind: 'idle' }
  | { kind: 'pending' }
  | { kind: 'valid' }
  | { kind: 'invalid'; message: string };

const TEMPLATES: ReadonlyArray<{ id: string; label: string; build: (bucket: string) => string }> = [
  {
    id: 'public-read',
    label: 'PublicRead',
    build: (b) =>
      formatJson({
        Version: '2012-10-17',
        Statement: [
          {
            Sid: 'PublicRead',
            Effect: 'Allow',
            Principal: '*',
            Action: ['s3:GetObject'],
            Resource: [`arn:aws:s3:::${b}/*`],
          },
        ],
      }),
  },
  {
    id: 'read-only-authenticated',
    label: 'ReadOnlyAuthenticated',
    build: (b) =>
      formatJson({
        Version: '2012-10-17',
        Statement: [
          {
            Sid: 'AuthenticatedReadOnly',
            Effect: 'Allow',
            Principal: { AWS: '*' },
            Action: ['s3:GetObject', 's3:ListBucket'],
            Resource: [`arn:aws:s3:::${b}`, `arn:aws:s3:::${b}/*`],
          },
        ],
      }),
  },
  {
    id: 'deny-all-except-user',
    label: 'DenyAllExceptUser',
    build: (b) =>
      formatJson({
        Version: '2012-10-17',
        Statement: [
          {
            Sid: 'DenyAllExceptOwner',
            Effect: 'Deny',
            Principal: '*',
            Action: ['s3:*'],
            Resource: [`arn:aws:s3:::${b}`, `arn:aws:s3:::${b}/*`],
            Condition: {
              StringNotEquals: {
                'aws:username': ['REPLACE_WITH_USERNAME'],
              },
            },
          },
        ],
      }),
  },
];

export function BucketPolicyTab({ bucket }: Props) {
  const policyQ = useQuery({
    queryKey: queryKeys.buckets.policy(bucket.name),
    queryFn: () => fetchBucketPolicyText(bucket.name),
    meta: { silent: true },
  });

  const initialText = useMemo(() => policyQ.data ?? '', [policyQ.data]);
  const [text, setText] = useState<string>(initialText);
  const [dryRun, setDryRun] = useState<DryRunState>({ kind: 'idle' });
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );
  const [saving, setSaving] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleteConfirm, setDeleteConfirm] = useState('');
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    setText(initialText);
    setDryRun({ kind: 'idle' });
    setServerError(null);
  }, [initialText]);

  const dirty = text.trim().length > 0 && text !== initialText;
  const hasStored = (policyQ.data ?? null) !== null;

  async function handleValidate() {
    if (text.trim().length === 0) {
      setDryRun({ kind: 'invalid', message: 'policy document is empty' });
      return;
    }
    setDryRun({ kind: 'pending' });
    try {
      const res = await dryRunBucketPolicy(bucket.name, text);
      if (res.valid) setDryRun({ kind: 'valid' });
      else setDryRun({ kind: 'invalid', message: res.message ?? 'invalid' });
    } catch (err) {
      const e = err as AdminApiError;
      setDryRun({ kind: 'invalid', message: e.message });
    }
  }

  async function handleSave() {
    if (text.trim().length === 0) {
      setServerError({ code: 'InvalidArgument', message: 'policy document is empty' });
      return;
    }
    setSaving(true);
    setServerError(null);
    try {
      await setBucketPolicy(bucket.name, text);
      showToast({ title: 'Bucket policy saved', description: bucket.name });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.policy(bucket.name),
      });
      setDryRun({ kind: 'valid' });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSaving(false);
    }
  }

  async function handleDelete() {
    setDeleting(true);
    setServerError(null);
    try {
      await deleteBucketPolicy(bucket.name);
      showToast({ title: 'Bucket policy deleted', description: bucket.name });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.policy(bucket.name),
      });
      setDeleteOpen(false);
      setDeleteConfirm('');
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setDeleting(false);
    }
  }

  function handleTemplate(id: string) {
    const tpl = TEMPLATES.find((t) => t.id === id);
    if (!tpl) return;
    setText(tpl.build(bucket.name));
    setDryRun({ kind: 'idle' });
    setServerError(null);
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Bucket policy</CardTitle>
        <CardDescription>
          IAM-style policy document. Validation runs the same parser the
          gateway uses at request-time. Edits do not take effect until saved.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex flex-wrap items-center gap-2">
          <div className="flex items-center gap-2">
            <Label className="text-xs uppercase tracking-wide text-muted-foreground">
              Generate template
            </Label>
            <Select onValueChange={handleTemplate} value="">
              <SelectTrigger className="h-9 w-[260px]">
                <SelectValue placeholder="Insert starter policy…" />
              </SelectTrigger>
              <SelectContent>
                {TEMPLATES.map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="ml-auto flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleValidate}
              disabled={dryRun.kind === 'pending' || policyQ.isPending}
            >
              {dryRun.kind === 'pending' && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Validate
            </Button>
          </div>
        </div>

        <Suspense
          fallback={<p className="text-sm text-muted-foreground">Loading editor…</p>}
        >
          <MonacoJsonEditor
            value={text}
            onChange={(next) => {
              setText(next);
              if (dryRun.kind !== 'idle' && dryRun.kind !== 'pending') {
                setDryRun({ kind: 'idle' });
              }
              setServerError(null);
            }}
            schema={{
              uri: 'https://strata.local/schemas/iam-policy.json',
              modelUri: `inmemory://policy/${bucket.name}.json`,
              schema: schemaJson as object,
            }}
            height={460}
          />
        </Suspense>

        {dryRun.kind === 'valid' && (
          <div className="flex items-start gap-2 rounded-md border border-green-500/40 bg-green-500/5 p-2 text-sm text-green-700 dark:text-green-400">
            <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            Policy parses cleanly.
          </div>
        )}
        {dryRun.kind === 'invalid' && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Validation failed</div>
              <div className="text-xs text-destructive/80">{dryRun.message}</div>
            </div>
          </div>
        )}

        {serverError && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">{serverError.code}</div>
              <div className="text-xs text-destructive/80">{serverError.message}</div>
            </div>
          </div>
        )}
      </CardContent>
      <CardFooter className="flex items-center justify-between gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="text-destructive"
          disabled={deleting || saving || policyQ.isPending || !hasStored}
          onClick={() => setDeleteOpen(true)}
        >
          Delete policy
        </Button>
        <Button
          type="button"
          size="sm"
          disabled={saving || policyQ.isPending || !dirty}
          onClick={handleSave}
        >
          {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
          Save policy
        </Button>
      </CardFooter>

      <Dialog
        open={deleteOpen}
        onOpenChange={(open) => {
          setDeleteOpen(open);
          if (!open) setDeleteConfirm('');
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete bucket policy</DialogTitle>
            <DialogDescription>
              This removes the policy from <code>{bucket.name}</code>. Type the
              bucket name to confirm.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="policy-delete-confirm">Bucket name</Label>
            <Input
              id="policy-delete-confirm"
              value={deleteConfirm}
              onChange={(e) => setDeleteConfirm(e.target.value)}
              placeholder={bucket.name}
              autoComplete="off"
            />
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setDeleteOpen(false)}
              disabled={deleting}
            >
              Cancel
            </Button>
            <Button
              type="button"
              variant="destructive"
              size="sm"
              onClick={handleDelete}
              disabled={deleting || deleteConfirm !== bucket.name}
            >
              {deleting && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function formatJson(v: unknown): string {
  return JSON.stringify(v, null, 2);
}
