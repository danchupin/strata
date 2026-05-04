import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useMutation, useQuery } from '@tanstack/react-query';
import {
  AlertCircle,
  ArrowLeft,
  CheckCircle2,
  KeyRound,
  Plus,
  RefreshCw,
  ShieldOff,
  Trash2,
  X,
} from 'lucide-react';

import {
  detachUserPolicy,
  fetchIAMAccessKeys,
  fetchIAMUser,
  fetchIAMUserPolicies,
  updateIAMAccessKeyDisabled,
  type AdminApiError,
  type IAMAccessKeySummary,
  type UserPolicyAttachment,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from '@/components/ui/tabs';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { AttachPolicyDialog } from '@/components/AttachPolicyDialog';
import { CreateAccessKeyDialog } from '@/components/CreateAccessKeyDialog';
import { DeleteAccessKeyDialog } from '@/components/DeleteAccessKeyDialog';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

function formatRelative(epochSec: number): string {
  if (!epochSec) return '—';
  const ms = epochSec * 1000;
  const diff = Date.now() - ms;
  const d = new Date(ms);
  const iso = d.toLocaleString();
  if (diff < 0 || diff > 30 * 86400 * 1000) return iso;
  const sec = Math.floor(diff / 1000);
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  return `${day}d ago`;
}

export function IAMUserDetailPage() {
  const params = useParams<{ userName: string }>();
  const userName = params.userName ?? '';
  const navigate = useNavigate();

  const userQ = useQuery({
    queryKey: queryKeys.iam.user(userName),
    queryFn: () => fetchIAMUser(userName),
    enabled: !!userName,
    meta: { label: 'iam user' },
    retry: false,
  });

  if (!userName) {
    return <div className="text-sm text-muted-foreground">Missing userName.</div>;
  }

  const userMissing =
    userQ.error instanceof Error &&
    'status' in (userQ.error as AdminApiError) &&
    (userQ.error as AdminApiError).status === 404;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div className="space-y-1">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => navigate('/iam')}
            className="-ml-2 h-7 px-2 text-muted-foreground"
          >
            <ArrowLeft className="mr-1 h-3.5 w-3.5" aria-hidden />
            Back to IAM
          </Button>
          <h1 className="text-2xl font-semibold tracking-tight">{userName}</h1>
          {userQ.data && (
            <p className="text-sm text-muted-foreground">
              <span className="font-mono text-xs">{userQ.data.user_id}</span> · path{' '}
              {userQ.data.path || '/'} · created{' '}
              <span title={new Date(userQ.data.created_at * 1000).toISOString()}>
                {formatRelative(userQ.data.created_at)}
              </span>
            </p>
          )}
        </div>
      </div>

      {userMissing && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">User not found</div>
              <div className="text-xs text-destructive/80">
                <Link to="/iam" className="underline">
                  Return to the IAM users list
                </Link>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {!userMissing && (
        <Tabs defaultValue="access-keys">
          <TabsList>
            <TabsTrigger value="access-keys">Access keys</TabsTrigger>
            <TabsTrigger value="policies">Policies</TabsTrigger>
          </TabsList>
          <TabsContent value="access-keys" className="mt-4">
            <AccessKeysTab userName={userName} />
          </TabsContent>
          <TabsContent value="policies" className="mt-4">
            <PoliciesTab userName={userName} />
          </TabsContent>
        </Tabs>
      )}
    </div>
  );
}

function PoliciesTab({ userName }: { userName: string }) {
  const [attachOpen, setAttachOpen] = useState(false);
  const [detachTarget, setDetachTarget] = useState<UserPolicyAttachment | null>(null);

  const q = useQuery({
    queryKey: queryKeys.iam.userPolicies(userName),
    queryFn: () => fetchIAMUserPolicies(userName),
    meta: { label: 'attached policies' },
  });

  const detachM = useMutation({
    mutationFn: (arn: string) => detachUserPolicy(userName, arn),
    onSuccess: (_data, arn) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.userPolicies(userName) });
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.policies });
      showToast({
        title: 'Policy detached',
        description: arn,
      });
      setDetachTarget(null);
    },
    onError: (err) => {
      const e = err as AdminApiError | Error;
      showToast({
        title: 'Failed to detach policy',
        description: e.message,
        variant: 'destructive',
      });
    },
  });

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: queryKeys.iam.userPolicies(userName) });
  }

  const policies: UserPolicyAttachment[] = q.data ?? [];
  const showSkeleton = q.isPending && !q.data;
  const errorMessage = !q.data && q.error instanceof Error ? q.error.message : null;
  const attachedArns = useMemo(() => policies.map((p) => p.arn), [policies]);

  return (
    <div className="space-y-4">
      <AttachPolicyDialog
        open={attachOpen}
        userName={userName}
        attachedArns={attachedArns}
        onOpenChange={setAttachOpen}
      />
      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load attached policies</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}
      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Attached policies</CardTitle>
            <CardDescription>
              {q.isFetching && !showSkeleton
                ? 'Refreshing…'
                : `${policies.length} ${policies.length === 1 ? 'policy' : 'policies'}`}
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching}
              aria-label="Refresh attached policies"
            >
              <RefreshCw
                className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
                aria-hidden
              />
              Refresh
            </Button>
            <Button type="button" onClick={() => setAttachOpen(true)}>
              <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              Attach policy
            </Button>
          </div>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4 sm:pl-6">Name</TableHead>
                  <TableHead>ARN</TableHead>
                  <TableHead>Path</TableHead>
                  <TableHead className="pr-4 text-right sm:pr-6">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {showSkeleton &&
                  Array.from({ length: 3 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={4} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}
                {!showSkeleton && policies.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={4} className="py-10 text-center">
                      <div className="space-y-2">
                        <KeyRound
                          className="mx-auto h-6 w-6 text-muted-foreground"
                          aria-hidden
                        />
                        <div className="text-sm font-medium">No policies attached</div>
                        <div className="text-xs text-muted-foreground">
                          Attach a managed policy to grant this user access.
                        </div>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={() => setAttachOpen(true)}
                          className="mt-2"
                        >
                          Attach a policy
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                )}
                {policies.map((p) => {
                  const isPending = detachM.isPending && detachTarget?.arn === p.arn;
                  return (
                    <TableRow key={p.arn}>
                      <TableCell className="pl-4 font-medium sm:pl-6">
                        {p.name || <span className="text-muted-foreground">—</span>}
                      </TableCell>
                      <TableCell className="font-mono text-xs">{p.arn}</TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {p.path || '/'}
                      </TableCell>
                      <TableCell className="pr-4 text-right sm:pr-6">
                        <Button
                          type="button"
                          variant="ghost"
                          size="sm"
                          className="text-destructive hover:text-destructive"
                          disabled={isPending || detachM.isPending}
                          onClick={() => {
                            if (
                              window.confirm(
                                `Detach policy ${p.name || p.arn} from ${userName}?`,
                              )
                            ) {
                              setDetachTarget(p);
                              detachM.mutate(p.arn);
                            }
                          }}
                          aria-label={`Detach ${p.name || p.arn}`}
                        >
                          <X className="h-3.5 w-3.5" aria-hidden />
                        </Button>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function AccessKeysTab({ userName }: { userName: string }) {
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

  const q = useQuery({
    queryKey: queryKeys.iam.accessKeys(userName),
    queryFn: () => fetchIAMAccessKeys(userName),
    meta: { label: 'access keys' },
  });

  const flip = useMutation({
    mutationFn: ({ id, disabled }: { id: string; disabled: boolean }) =>
      updateIAMAccessKeyDisabled(id, disabled),
    onSuccess: (_data, vars) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.iam.accessKeys(userName) });
      showToast({
        title: vars.disabled ? 'Access key disabled' : 'Access key enabled',
        description: vars.id,
      });
    },
    onError: (err) => {
      const e = err as AdminApiError | Error;
      showToast({
        title: 'Failed to update access key',
        description: e.message,
        variant: 'destructive',
      });
    },
  });

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: queryKeys.iam.accessKeys(userName) });
  }

  const keys: IAMAccessKeySummary[] = q.data ?? [];
  const showSkeleton = q.isPending && !q.data;
  const errorMessage = !q.data && q.error instanceof Error ? q.error.message : null;

  const sorted = useMemo(
    () => [...keys].sort((a, b) => a.access_key_id.localeCompare(b.access_key_id)),
    [keys],
  );

  return (
    <div className="space-y-4">
      <CreateAccessKeyDialog
        open={createOpen}
        userName={userName}
        onOpenChange={setCreateOpen}
      />
      <DeleteAccessKeyDialog
        open={deleteTarget !== null}
        userName={userName}
        accessKeyID={deleteTarget}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null);
        }}
      />
      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load access keys</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Access keys</CardTitle>
            <CardDescription>
              {q.isFetching && !showSkeleton
                ? 'Refreshing…'
                : `${keys.length} ${keys.length === 1 ? 'key' : 'keys'}`}
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching}
              aria-label="Refresh access keys"
            >
              <RefreshCw
                className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
                aria-hidden
              />
              Refresh
            </Button>
            <Button type="button" onClick={() => setCreateOpen(true)}>
              <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              Create access key
            </Button>
          </div>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4 sm:pl-6">Access key ID</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="pr-4 text-right sm:pr-6">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {showSkeleton &&
                  Array.from({ length: 3 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={4} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}
                {!showSkeleton && sorted.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={4} className="py-10 text-center">
                      <div className="space-y-2">
                        <KeyRound
                          className="mx-auto h-6 w-6 text-muted-foreground"
                          aria-hidden
                        />
                        <div className="text-sm font-medium">No access keys</div>
                        <div className="text-xs text-muted-foreground">
                          Mint a new key to issue SigV4 credentials for this user.
                        </div>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={() => setCreateOpen(true)}
                          className="mt-2"
                        >
                          Create your first access key
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                )}
                {sorted.map((k) => (
                  <TableRow key={k.access_key_id}>
                    <TableCell className="pl-4 font-mono text-xs sm:pl-6">
                      {k.access_key_id}
                    </TableCell>
                    <TableCell>
                      {k.disabled ? (
                        <span className="inline-flex items-center gap-1 rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-xs text-amber-700 dark:text-amber-300">
                          <ShieldOff className="h-3 w-3" aria-hidden />
                          Disabled
                        </span>
                      ) : (
                        <span className="inline-flex items-center gap-1 rounded-full border border-emerald-500/40 bg-emerald-500/10 px-2 py-0.5 text-xs text-emerald-700 dark:text-emerald-400">
                          <CheckCircle2 className="h-3 w-3" aria-hidden />
                          Active
                        </span>
                      )}
                    </TableCell>
                    <TableCell
                      title={k.created_at ? new Date(k.created_at * 1000).toISOString() : ''}
                    >
                      {formatRelative(k.created_at)}
                    </TableCell>
                    <TableCell className="pr-4 text-right sm:pr-6">
                      <div className="flex justify-end gap-1.5">
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          disabled={flip.isPending}
                          onClick={() =>
                            flip.mutate({ id: k.access_key_id, disabled: !k.disabled })
                          }
                        >
                          {k.disabled ? 'Enable' : 'Disable'}
                        </Button>
                        <Button
                          type="button"
                          variant="ghost"
                          size="sm"
                          className="text-destructive hover:text-destructive"
                          onClick={() => setDeleteTarget(k.access_key_id)}
                          aria-label={`Delete access key ${k.access_key_id}`}
                        >
                          <Trash2 className="h-3.5 w-3.5" aria-hidden />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
