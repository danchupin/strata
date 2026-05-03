import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { AlertCircle, Loader2, Search, ShieldCheck } from 'lucide-react';

import {
  attachUserPolicy,
  fetchManagedPolicies,
  type AdminApiError,
  type ManagedPolicySummary,
} from '@/api/client';
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
import { Skeleton } from '@/components/ui/skeleton';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

interface Props {
  open: boolean;
  userName: string;
  attachedArns: string[];
  onOpenChange: (open: boolean) => void;
}

// AttachPolicyDialog renders a searchable list of every managed policy and
// attaches the operator's pick to userName via POST .../policies. Already-
// attached policies are visually disabled — the server would reject them with
// 409 EntityAlreadyExists anyway, but greying out keeps the operator from
// trying.
export function AttachPolicyDialog({ open, userName, attachedArns, onOpenChange }: Props) {
  const [search, setSearch] = useState('');
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(null);
  const [pendingArn, setPendingArn] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setSearch('');
      setServerError(null);
      setPendingArn(null);
    }
  }, [open]);

  const policiesQ = useQuery({
    queryKey: queryKeys.iam.policies,
    queryFn: fetchManagedPolicies,
    enabled: open,
    meta: { label: 'managed policies' },
  });

  const attached = useMemo(() => new Set(attachedArns), [attachedArns]);

  const filtered = useMemo<ManagedPolicySummary[]>(() => {
    const all = policiesQ.data ?? [];
    const q = search.trim().toLowerCase();
    if (!q) return all;
    return all.filter(
      (p) =>
        p.name.toLowerCase().includes(q) ||
        p.arn.toLowerCase().includes(q) ||
        (p.description?.toLowerCase().includes(q) ?? false),
    );
  }, [policiesQ.data, search]);

  const attachM = useMutation({
    mutationFn: (arn: string) => attachUserPolicy(userName, arn),
    onSuccess: (_data, arn) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.userPolicies(userName) });
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.policies });
      showToast({
        title: 'Policy attached',
        description: arn,
      });
      setPendingArn(null);
      onOpenChange(false);
    },
    onError: (err) => {
      const e = err as AdminApiError | Error;
      const code = (e as AdminApiError).code ?? 'Error';
      setServerError({ code, message: e.message });
      setPendingArn(null);
    },
  });

  function handleAttach(arn: string) {
    setServerError(null);
    setPendingArn(arn);
    attachM.mutate(arn);
  }

  const showSkeleton = policiesQ.isPending && !policiesQ.data;
  const errorMessage = !policiesQ.data && policiesQ.error instanceof Error ? policiesQ.error.message : null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Attach managed policy</DialogTitle>
          <DialogDescription>
            Pick a managed policy to attach to <strong>{userName}</strong>.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div className="relative">
            <Search className="absolute left-3 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" aria-hidden />
            <Input
              type="search"
              placeholder="Search by name, ARN, or description…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="pl-9"
            />
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
          {errorMessage && (
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">Failed to load managed policies</div>
                <div className="text-xs text-destructive/80">{errorMessage}</div>
              </div>
            </div>
          )}
          <div className="max-h-80 overflow-y-auto rounded-md border">
            {showSkeleton && (
              <div className="space-y-2 p-3">
                {Array.from({ length: 4 }).map((_, i) => (
                  <Skeleton key={`sk-${i}`} className="h-10 w-full" />
                ))}
              </div>
            )}
            {!showSkeleton && filtered.length === 0 && (
              <div className="p-6 text-center text-sm text-muted-foreground">
                {search.trim()
                  ? 'No matching managed policies.'
                  : 'No managed policies yet. Create one from the IAM Policies tab first.'}
              </div>
            )}
            {!showSkeleton &&
              filtered.map((p) => {
                const isAttached = attached.has(p.arn);
                const isPending = pendingArn === p.arn;
                return (
                  <button
                    key={p.arn}
                    type="button"
                    disabled={isAttached || attachM.isPending}
                    onClick={() => handleAttach(p.arn)}
                    className={cn(
                      'flex w-full items-start justify-between gap-3 border-b px-3 py-2.5 text-left transition last:border-b-0',
                      'hover:bg-muted/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
                      (isAttached || attachM.isPending) && 'cursor-not-allowed opacity-60 hover:bg-transparent',
                    )}
                  >
                    <div className="space-y-0.5">
                      <div className="flex items-center gap-2 text-sm font-medium">
                        <ShieldCheck className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
                        {p.name}
                      </div>
                      <div className="font-mono text-xs text-muted-foreground">{p.arn}</div>
                      {p.description && (
                        <div className="text-xs text-muted-foreground">{p.description}</div>
                      )}
                    </div>
                    <div className="shrink-0 text-xs">
                      {isAttached ? (
                        <span className="rounded-full border border-emerald-500/40 bg-emerald-500/10 px-2 py-0.5 text-emerald-700 dark:text-emerald-300">
                          Attached
                        </span>
                      ) : isPending ? (
                        <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
                      ) : (
                        <span className="text-muted-foreground">Attach</span>
                      )}
                    </div>
                  </button>
                );
              })}
          </div>
        </div>
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={attachM.isPending}
          >
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
