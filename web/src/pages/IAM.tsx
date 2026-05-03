import { useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { AlertCircle, Pencil, Plus, RefreshCw, Search, Trash2 } from 'lucide-react';

import {
  fetchIAMUsers,
  fetchManagedPolicies,
  type IAMUserSummary,
  type ManagedPolicySummary,
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
import { Input } from '@/components/ui/input';
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
import { CreateIAMUserDialog } from '@/components/CreateIAMUserDialog';
import { DeleteIAMUserDialog } from '@/components/DeleteIAMUserDialog';
import { ManagedPolicyEditorDialog } from '@/components/ManagedPolicyEditorDialog';
import { DeleteManagedPolicyDialog } from '@/components/DeleteManagedPolicyDialog';
import { cn } from '@/lib/utils';

const PAGE_SIZE = 50;
const SEARCH_DEBOUNCE_MS = 300;

function useDebounced<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}

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

export function IAMPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">IAM</h1>
        <p className="text-sm text-muted-foreground">
          Operator-managed users, access keys, and managed policies.
        </p>
      </div>
      <Tabs defaultValue="users">
        <TabsList>
          <TabsTrigger value="users">Users</TabsTrigger>
          <TabsTrigger value="access-keys">Access keys</TabsTrigger>
          <TabsTrigger value="policies">Policies</TabsTrigger>
        </TabsList>
        <TabsContent value="users" className="mt-4">
          <UsersTab />
        </TabsContent>
        <TabsContent value="access-keys" className="mt-4">
          <PlaceholderTab
            title="Access keys"
            description="Open a user from the Users tab to manage their access keys."
          />
        </TabsContent>
        <TabsContent value="policies" className="mt-4">
          <PoliciesTab />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function PlaceholderTab({
  title,
  description,
}: {
  title: string;
  description: string;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{title}</CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent className="text-sm text-muted-foreground">
        Tab is intentionally empty — the route exists so navigation stays
        stable while later stories land.
      </CardContent>
    </Card>
  );
}

function UsersTab() {
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebounced(search, SEARCH_DEBOUNCE_MS);
  const [page, setPage] = useState(1);
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<IAMUserSummary | null>(null);

  useEffect(() => {
    setPage(1);
  }, [debouncedSearch]);

  const params = useMemo(
    () => ({ query: debouncedSearch || undefined, page, pageSize: PAGE_SIZE }),
    [debouncedSearch, page],
  );

  const q = useQuery({
    queryKey: queryKeys.iam.users(debouncedSearch, page, PAGE_SIZE),
    queryFn: () => fetchIAMUsers(params),
    placeholderData: keepPreviousData,
    meta: { label: 'iam users' },
  });

  const users: IAMUserSummary[] = q.data?.users ?? [];
  const total = q.data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const showSkeleton = q.isPending && !q.data;
  const errorMessage = !q.data && q.error instanceof Error ? q.error.message : null;

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: ['iam', 'users'] });
  }

  return (
    <div className="space-y-4">
      <CreateIAMUserDialog open={createOpen} onOpenChange={setCreateOpen} />
      <DeleteIAMUserDialog
        open={deleteTarget !== null}
        user={deleteTarget}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null);
        }}
      />

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load users</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Users</CardTitle>
            <CardDescription>
              {q.isFetching && !showSkeleton
                ? 'Refreshing…'
                : `${total} ${total === 1 ? 'user' : 'users'} total`}
            </CardDescription>
          </div>
          <div className="flex items-center gap-2">
            <div className="relative">
              <Search
                className="pointer-events-none absolute left-2 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
                aria-hidden
              />
              <Input
                aria-label="Search users"
                placeholder="Search users…"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                className="h-9 w-56 pl-8"
              />
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={handleRefresh}
              disabled={q.isFetching}
              aria-label="Refresh users"
            >
              <RefreshCw
                className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
                aria-hidden
              />
              Refresh
            </Button>
            <Button type="button" onClick={() => setCreateOpen(true)}>
              <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              Create user
            </Button>
          </div>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4 sm:pl-6">User name</TableHead>
                  <TableHead>User ID</TableHead>
                  <TableHead>Path</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right">Access keys</TableHead>
                  <TableHead className="pr-4 text-right sm:pr-6">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {showSkeleton &&
                  Array.from({ length: 4 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={6} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}
                {!showSkeleton && users.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={6} className="py-10 text-center">
                      <div className="space-y-2">
                        <div className="text-sm font-medium">No users</div>
                        <div className="text-xs text-muted-foreground">
                          {debouncedSearch
                            ? `No users match "${debouncedSearch}".`
                            : 'No IAM users have been created yet.'}
                        </div>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={() => setCreateOpen(true)}
                          className="mt-2"
                        >
                          Create your first user
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                )}
                {users.map((u) => (
                  <TableRow key={u.user_name}>
                    <TableCell className="pl-4 font-medium sm:pl-6">
                      <Link
                        to={`/iam/users/${encodeURIComponent(u.user_name)}`}
                        className="hover:underline"
                      >
                        {u.user_name}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {u.user_id}
                    </TableCell>
                    <TableCell className="text-muted-foreground">{u.path || '/'}</TableCell>
                    <TableCell
                      title={u.created_at ? new Date(u.created_at * 1000).toISOString() : ''}
                    >
                      {formatRelative(u.created_at)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {u.access_key_count}
                    </TableCell>
                    <TableCell className="pr-4 text-right sm:pr-6">
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={() => setDeleteTarget(u)}
                        className="text-destructive hover:text-destructive"
                        aria-label={`Delete user ${u.user_name}`}
                      >
                        <Trash2 className="h-3.5 w-3.5" aria-hidden />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          {total > 0 && totalPages > 1 && (
            <div className="flex flex-col items-center justify-between gap-2 border-t px-4 py-3 text-sm text-muted-foreground sm:flex-row sm:px-6">
              <div>
                Page {page} of {totalPages} · {total} total
              </div>
              <div className="flex items-center gap-1.5">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setPage((p) => Math.max(1, p - 1))}
                  disabled={page <= 1 || q.isFetching}
                >
                  Previous
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                  disabled={page >= totalPages || q.isFetching}
                >
                  Next
                </Button>
              </div>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function PoliciesTab() {
  const [editorOpen, setEditorOpen] = useState(false);
  const [editing, setEditing] = useState<ManagedPolicySummary | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<ManagedPolicySummary | null>(null);

  const q = useQuery({
    queryKey: queryKeys.iam.policies,
    queryFn: () => fetchManagedPolicies(),
    meta: { label: 'managed policies' },
  });

  const policies = q.data ?? [];
  const showSkeleton = q.isPending && !q.data;
  const errorMessage = !q.data && q.error instanceof Error ? q.error.message : null;

  function handleRefresh() {
    void queryClient.invalidateQueries({ queryKey: queryKeys.iam.policies });
  }

  function openCreate() {
    setEditing(null);
    setEditorOpen(true);
  }

  function openEdit(policy: ManagedPolicySummary) {
    setEditing(policy);
    setEditorOpen(true);
  }

  return (
    <div className="space-y-4">
      <ManagedPolicyEditorDialog
        open={editorOpen}
        editing={editing}
        onOpenChange={(open) => {
          setEditorOpen(open);
          if (!open) setEditing(null);
        }}
      />
      <DeleteManagedPolicyDialog
        open={deleteTarget !== null}
        policy={deleteTarget}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null);
        }}
      />

      {errorMessage && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="flex items-start gap-2 py-4 text-sm text-destructive">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
            <div>
              <div className="font-medium">Failed to load managed policies</div>
              <div className="text-xs text-destructive/80">{errorMessage}</div>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Managed policies</CardTitle>
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
              aria-label="Refresh managed policies"
            >
              <RefreshCw
                className={cn('mr-1.5 h-3.5 w-3.5', q.isFetching && 'animate-spin')}
                aria-hidden
              />
              Refresh
            </Button>
            <Button type="button" onClick={openCreate}>
              <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden />
              Create policy
            </Button>
          </div>
        </CardHeader>
        <CardContent className="px-0 sm:px-0">
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4 sm:pl-6">Name</TableHead>
                  <TableHead>Path</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead>Updated</TableHead>
                  <TableHead className="text-right">Attachments</TableHead>
                  <TableHead className="pr-4 text-right sm:pr-6">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {showSkeleton &&
                  Array.from({ length: 3 }).map((_, i) => (
                    <TableRow key={`sk-${i}`}>
                      <TableCell colSpan={6} className="py-3">
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}
                {!showSkeleton && policies.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={6} className="py-10 text-center">
                      <div className="space-y-2">
                        <div className="text-sm font-medium">No managed policies</div>
                        <div className="text-xs text-muted-foreground">
                          Create a managed policy to attach to one or more IAM users.
                        </div>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={openCreate}
                          className="mt-2"
                        >
                          Create your first policy
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                )}
                {policies.map((p) => (
                  <TableRow key={p.arn}>
                    <TableCell className="pl-4 font-medium sm:pl-6">
                      <div className="font-medium">{p.name}</div>
                      <div className="font-mono text-xs text-muted-foreground">
                        {p.arn}
                      </div>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{p.path || '/'}</TableCell>
                    <TableCell
                      title={p.created_at ? new Date(p.created_at * 1000).toISOString() : ''}
                    >
                      {formatRelative(p.created_at)}
                    </TableCell>
                    <TableCell
                      title={p.updated_at ? new Date(p.updated_at * 1000).toISOString() : ''}
                    >
                      {formatRelative(p.updated_at)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {p.attachment_count}
                    </TableCell>
                    <TableCell className="pr-4 text-right sm:pr-6">
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={() => openEdit(p)}
                        aria-label={`Edit policy ${p.name}`}
                      >
                        <Pencil className="h-3.5 w-3.5" aria-hidden />
                      </Button>
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={() => setDeleteTarget(p)}
                        className="text-destructive hover:text-destructive"
                        aria-label={`Delete policy ${p.name}`}
                      >
                        <Trash2 className="h-3.5 w-3.5" aria-hidden />
                      </Button>
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
