import { Suspense, lazy, useEffect, useMemo, useState } from 'react';
import { AlertCircle, Loader2 } from 'lucide-react';

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
import {
  createManagedPolicy,
  updateManagedPolicyDocument,
  type AdminApiError,
  type ManagedPolicySummary,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import schemaJson from '@/schemas/iam-policy.json';

// Reuse the lazy Monaco chunk shared with Lifecycle / CORS / Bucket Policy
// (US-004/US-005/US-006) so the per-page bundle stays under the 500 KiB
// gzipped budget.
const MonacoJsonEditor = lazy(() => import('./MonacoJsonEditor'));

// Mirror of internal/adminapi/iam_policies.go iamPolicyNamePattern.
const POLICY_NAME_RE = /^[A-Za-z0-9_+=,.@-]{1,128}$/;

function nameViolation(raw: string): string | null {
  const name = raw.trim();
  if (name.length === 0) return null;
  if (!POLICY_NAME_RE.test(name)) {
    return '1–128 chars: letters, digits, and any of _+=,.@-';
  }
  return null;
}

function pathViolation(raw: string): string | null {
  const p = raw.trim();
  if (p === '' || p === '/') return null;
  if (!p.startsWith('/') || !p.endsWith('/')) return 'must begin and end with "/"';
  return null;
}

const STARTER_DOC = JSON.stringify(
  {
    Version: '2012-10-17',
    Statement: [
      {
        Sid: 'AllowReadAll',
        Effect: 'Allow',
        Action: ['s3:GetObject', 's3:ListBucket'],
        Resource: ['arn:aws:s3:::*'],
      },
    ],
  },
  null,
  2,
);

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // editing is set when the dialog opens for a specific policy (edits document
  // only — name + path are immutable on edit, mirroring the backend route which
  // only exposes UpdateManagedPolicyDocument).
  editing?: ManagedPolicySummary | null;
}

export function ManagedPolicyEditorDialog({ open, onOpenChange, editing }: Props) {
  const isEdit = editing != null;

  const [name, setName] = useState('');
  const [path, setPath] = useState('/');
  const [description, setDescription] = useState('');
  const [document, setDocument] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );

  useEffect(() => {
    if (!open) return;
    if (isEdit && editing) {
      setName(editing.name);
      setPath(editing.path || '/');
      setDescription(editing.description ?? '');
      setDocument(editing.document);
    } else {
      setName('');
      setPath('/');
      setDescription('');
      setDocument(STARTER_DOC);
    }
    setSubmitting(false);
    setServerError(null);
  }, [open, isEdit, editing]);

  const nameError = useMemo(() => (isEdit ? null : nameViolation(name)), [name, isEdit]);
  const pathError = useMemo(() => (isEdit ? null : pathViolation(path)), [path, isEdit]);

  const submitDisabled =
    submitting ||
    document.trim().length === 0 ||
    (!isEdit && (name.trim().length === 0 || nameError !== null || pathError !== null));

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    setServerError(null);
    setSubmitting(true);
    try {
      if (isEdit && editing) {
        await updateManagedPolicyDocument(editing.arn, document);
        showToast({ title: `Policy ${editing.name} updated` });
      } else {
        const body = {
          name: name.trim(),
          path: path.trim() && path.trim() !== '/' ? path.trim() : undefined,
          description: description.trim() || undefined,
          document,
        };
        const created = await createManagedPolicy(body);
        showToast({ title: `Policy ${created.name} created` });
      }
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.policies });
      onOpenChange(false);
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>{isEdit ? 'Edit managed policy' : 'Create managed policy'}</DialogTitle>
          <DialogDescription>
            {isEdit
              ? 'Updates the policy document. Name and path are immutable; create a new policy if you need to rename.'
              : 'Mints a managed policy in the strata IAM namespace. Attach to users from the user detail page.'}
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="mp-name">Name</Label>
              <Input
                id="mp-name"
                autoFocus={!isEdit}
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="ReadOnly"
                disabled={isEdit || submitting}
                aria-invalid={nameError ? 'true' : 'false'}
              />
              {nameError ? (
                <p className="text-xs text-destructive">{nameError}</p>
              ) : (
                <p className="text-xs text-muted-foreground">
                  1–128 chars; letters, digits, and any of <code>_+=,.@-</code>.
                </p>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="mp-path">Path</Label>
              <Input
                id="mp-path"
                value={path}
                onChange={(e) => setPath(e.target.value)}
                placeholder="/"
                disabled={isEdit || submitting}
                aria-invalid={pathError ? 'true' : 'false'}
              />
              {pathError ? (
                <p className="text-xs text-destructive">{pathError}</p>
              ) : (
                <p className="text-xs text-muted-foreground">
                  Defaults to <code>/</code>. Custom paths must begin and end with <code>/</code>.
                </p>
              )}
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="mp-desc">Description</Label>
            <Input
              id="mp-desc"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="One-line summary"
              disabled={isEdit || submitting}
            />
          </div>

          <div className="space-y-1.5">
            <Label>Policy document</Label>
            <Suspense
              fallback={<p className="text-sm text-muted-foreground">Loading editor…</p>}
            >
              <MonacoJsonEditor
                value={document}
                onChange={(next) => {
                  setDocument(next);
                  setServerError(null);
                }}
                schema={{
                  uri: 'https://strata.local/schemas/iam-policy.json',
                  modelUri: `inmemory://managed-policy/${name || 'new'}.json`,
                  schema: schemaJson as object,
                }}
                height={360}
              />
            </Suspense>
          </div>

          {serverError && (
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">{serverError.code}</div>
                <div className="text-xs text-destructive/80">{serverError.message}</div>
              </div>
            </div>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={submitDisabled}>
              {submitting && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
              {isEdit ? 'Save document' : 'Create policy'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
