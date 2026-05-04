import { useEffect, useMemo, useState } from 'react';
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
  createIAMUser,
  type AdminApiError,
  type CreateIAMUserBody,
  type IAMUserSummary,
} from '@/api/client';
import { queryClient } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

// Mirror of internal/adminapi/iam_users.go iamUserNamePattern.
const USER_NAME_RE = /^[a-zA-Z0-9_+=,.@-]{1,64}$/;

function userNameViolation(raw: string): string | null {
  const name = raw.trim();
  if (name.length === 0) return null;
  if (!USER_NAME_RE.test(name)) {
    return '1–64 chars: letters, digits, and any of _+=,.@-';
  }
  return null;
}

function pathViolation(raw: string): string | null {
  const p = raw.trim();
  if (p === '' || p === '/') return null;
  if (!p.startsWith('/') || !p.endsWith('/')) return 'must begin and end with "/"';
  return null;
}

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated?: (user: IAMUserSummary) => void;
}

export function CreateIAMUserDialog({ open, onOpenChange, onCreated }: Props) {
  const [userName, setUserName] = useState('');
  const [path, setPath] = useState('/');
  const [submitting, setSubmitting] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(null);

  useEffect(() => {
    if (open) {
      setUserName('');
      setPath('/');
      setSubmitting(false);
      setServerError(null);
    }
  }, [open]);

  const nameError = useMemo(() => userNameViolation(userName), [userName]);
  const pathError = useMemo(() => pathViolation(path), [path]);
  const submitDisabled =
    submitting || userName.trim().length === 0 || nameError !== null || pathError !== null;

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitDisabled) return;
    setServerError(null);
    setSubmitting(true);
    const body: CreateIAMUserBody = { user_name: userName.trim() };
    if (path.trim() && path.trim() !== '/') body.path = path.trim();
    try {
      const created = await createIAMUser(body);
      void queryClient.invalidateQueries({ queryKey: ['iam', 'users'] });
      showToast({ title: `User ${created.user_name} created` });
      onCreated?.(created);
      onOpenChange(false);
    } catch (err) {
      const e = err as AdminApiError | Error;
      const code = (e as AdminApiError).code ?? 'Error';
      setServerError({ code, message: e.message });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Create IAM user</DialogTitle>
          <DialogDescription>
            Mints a fresh IAM principal. Access keys can be created from the
            user detail page after the user exists.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="iu-name">User name</Label>
            <Input
              id="iu-name"
              autoFocus
              value={userName}
              onChange={(e) => setUserName(e.target.value)}
              placeholder="alice"
              aria-invalid={nameError ? 'true' : 'false'}
              disabled={submitting}
            />
            {nameError ? (
              <p className="text-xs text-destructive">{nameError}</p>
            ) : (
              <p className="text-xs text-muted-foreground">
                1–64 chars; letters, digits, and any of <code>_+=,.@-</code>.
              </p>
            )}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="iu-path">Path</Label>
            <Input
              id="iu-path"
              value={path}
              onChange={(e) => setPath(e.target.value)}
              placeholder="/"
              aria-invalid={pathError ? 'true' : 'false'}
              disabled={submitting}
            />
            {pathError ? (
              <p className="text-xs text-destructive">{pathError}</p>
            ) : (
              <p className="text-xs text-muted-foreground">
                Defaults to <code>/</code>. Custom paths must begin and end with <code>/</code>.
              </p>
            )}
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
              Create user
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
