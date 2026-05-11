import { useEffect, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { AlertCircle, Loader2, RotateCw, ShieldAlert } from 'lucide-react';

import {
  fetchSettings,
  fetchSettingsDataBackend,
  rotateJWTSecret,
  type AdminApiError,
  type S3BackendSettings,
  type SettingsResponse,
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
import { Skeleton } from '@/components/ui/skeleton';
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from '@/components/ui/tabs';
import { showToast } from '@/lib/toast-store';

function KV({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="grid grid-cols-3 gap-2 border-b border-border/50 py-2 text-sm last:border-b-0">
      <div className="col-span-1 text-muted-foreground">{label}</div>
      <div className="col-span-2 font-mono text-xs break-all">{value}</div>
    </div>
  );
}

function formatDuration(ms: number): string {
  if (!ms) return '—';
  const sec = Math.round(ms / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.round(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.round(min / 60);
  if (hr < 48) return `${hr}h`;
  const day = Math.round(hr / 24);
  return `${day}d`;
}

function ClusterTab({ s }: { s: SettingsResponse }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Cluster identity</CardTitle>
        <CardDescription>
          Read-only. Set via STRATA_CLUSTER_NAME / STRATA_REGION on the gateway.
        </CardDescription>
      </CardHeader>
      <CardContent className="px-6 py-2">
        <KV label="Cluster name" value={s.settings.cluster_name} />
        <KV label="Region" value={s.settings.region} />
        <KV label="Version" value={s.settings.version} />
        <KV
          label="Prometheus URL"
          value={s.settings.prometheus_url || <span className="text-muted-foreground">unset</span>}
        />
        <KV
          label="Heartbeat interval"
          value={formatDuration(s.settings.heartbeat_interval_ms)}
        />
        <KV
          label="Audit retention"
          value={formatDuration(s.settings.audit_retention_ms)}
        />
      </CardContent>
    </Card>
  );
}

function ConsoleTab({ s }: { s: SettingsResponse }) {
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [confirm, setConfirm] = useState('');

  const rotateMutation = useMutation({
    mutationFn: rotateJWTSecret,
    onSuccess: (resp) => {
      showToast({
        title: 'JWT secret rotated',
        description: `Persisted to ${resp.file}. You will be redirected to /login.`,
      });
      setConfirmOpen(false);
      setConfirm('');
      // Eager-invalidate so the next render re-fetches /settings; the next
      // fetch will trigger 401 and RequireAuth bounces to /login.
      void queryClient.invalidateQueries({ queryKey: queryKeys.settings.all });
      setTimeout(() => {
        window.location.href = '/login';
      }, 1500);
    },
    onError: (err: AdminApiError) => {
      showToast({
        title: `Rotate failed (${err.code ?? 'Error'})`,
        description: err.message,
        variant: 'destructive',
      });
    },
  });

  useEffect(() => {
    if (!confirmOpen) setConfirm('');
  }, [confirmOpen]);

  const armed = confirm.trim().toLowerCase() === 'rotate';

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>Console</CardTitle>
          <CardDescription>Operator-console knobs.</CardDescription>
        </CardHeader>
        <CardContent className="px-6 py-2">
          <KV label="Default theme" value={s.settings.console_theme_default} />
          <KV
            label="JWT secret"
            value={
              <span className="inline-flex items-center gap-2">
                <span>{s.settings.jwt_secret}</span>
                {s.settings.jwt_ephemeral && (
                  <span className="inline-flex items-center gap-1 rounded-md border border-amber-500/40 bg-amber-500/10 px-1.5 py-0.5 text-[11px] text-amber-700 dark:text-amber-300">
                    <ShieldAlert className="h-3 w-3" />
                    sessions invalidate on restart
                  </span>
                )}
              </span>
            }
          />
          <KV label="JWT secret file" value={s.settings.jwt_secret_file || '—'} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Rotate JWT secret</CardTitle>
          <CardDescription>
            Mints a fresh HS256 key, persists it to the configured secret file,
            and invalidates every active console session. You will be logged
            out immediately.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Button
            variant="destructive"
            onClick={() => setConfirmOpen(true)}
            disabled={rotateMutation.isPending}
          >
            <RotateCw className="mr-2 h-4 w-4" aria-hidden />
            Rotate JWT secret
          </Button>
        </CardContent>
      </Card>

      <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Rotate JWT secret</DialogTitle>
            <DialogDescription>
              All active console sessions across the cluster will be
              invalidated. You will be redirected to /login.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <Label htmlFor="rotate-confirm">
              Type <code className="font-mono text-xs">rotate</code> to confirm
            </Label>
            <Input
              id="rotate-confirm"
              autoFocus
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              placeholder="rotate"
              disabled={rotateMutation.isPending}
            />
            {rotateMutation.isError && (
              <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
                <div>
                  <div className="font-medium">
                    {(rotateMutation.error as AdminApiError)?.code ?? 'Error'}
                  </div>
                  <div className="text-xs text-destructive/80">
                    {rotateMutation.error?.message}
                  </div>
                </div>
              </div>
            )}
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setConfirmOpen(false)}
              disabled={rotateMutation.isPending}
            >
              Cancel
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={() => rotateMutation.mutate()}
              disabled={!armed || rotateMutation.isPending}
            >
              {rotateMutation.isPending && (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              )}
              Rotate
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function S3BackendCard({ s3 }: { s3: S3BackendSettings }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>S3 Backend</CardTitle>
        <CardDescription>
          STRATA_S3_BACKEND_* config. Access keys masked — never echoed.
        </CardDescription>
      </CardHeader>
      <CardContent className="px-6 py-2">
        <KV label="Endpoint" value={s3.endpoint || <em>SDK default</em>} />
        <KV label="Region" value={s3.region} />
        <KV label="Bucket" value={s3.bucket} />
        <KV label="Force path style" value={String(s3.force_path_style)} />
        <KV label="Part size" value={s3.part_size ? `${s3.part_size} bytes` : '—'} />
        <KV label="Upload concurrency" value={String(s3.upload_concurrency)} />
        <KV label="Max retries" value={String(s3.max_retries)} />
        <KV label="Op timeout" value={s3.op_timeout_secs ? `${s3.op_timeout_secs}s` : '—'} />
        <KV label="SSE mode" value={s3.sse_mode || 'passthrough'} />
        <KV
          label="SSE KMS key"
          value={s3.sse_kms_key_id || <span className="text-muted-foreground">—</span>}
        />
        <KV label="Access key" value={s3.access_key_set ? '<set>' : <span className="text-muted-foreground">unset</span>} />
        <KV label="Secret key" value={s3.secret_key_set ? '<set>' : <span className="text-muted-foreground">unset</span>} />
      </CardContent>
    </Card>
  );
}

function BackendsTab({ s }: { s: SettingsResponse }) {
  const meta = s.settings.meta_backend;
  const data = s.settings.data_backend;
  const dataBackendQuery = useQuery({
    queryKey: queryKeys.settings.dataBackend,
    queryFn: fetchSettingsDataBackend,
    meta: { label: 'data-backend settings' },
  });

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>Active backends</CardTitle>
          <CardDescription>
            STRATA_META_BACKEND / STRATA_DATA_BACKEND.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-6 py-2">
          <KV label="Meta backend" value={meta} />
          <KV label="Data backend" value={data} />
        </CardContent>
      </Card>

      {meta === 'cassandra' && (
        <Card>
          <CardHeader>
            <CardTitle>Cassandra / ScyllaDB</CardTitle>
          </CardHeader>
          <CardContent className="px-6 py-2">
            <KV label="Hosts" value={s.backends.cassandra.hosts.join(', ') || '—'} />
            <KV label="Keyspace" value={s.backends.cassandra.keyspace} />
            <KV label="Local DC" value={s.backends.cassandra.local_dc} />
            <KV label="Replication" value={s.backends.cassandra.replication} />
            <KV
              label="Username"
              value={s.backends.cassandra.username || <span className="text-muted-foreground">unset</span>}
            />
          </CardContent>
        </Card>
      )}

      {meta === 'tikv' && (
        <Card>
          <CardHeader>
            <CardTitle>TiKV</CardTitle>
          </CardHeader>
          <CardContent className="px-6 py-2">
            <KV
              label="PD endpoints"
              value={s.backends.tikv.endpoints.join(', ') || '—'}
            />
          </CardContent>
        </Card>
      )}

      {data === 'rados' && (
        <Card>
          <CardHeader>
            <CardTitle>RADOS</CardTitle>
          </CardHeader>
          <CardContent className="px-6 py-2">
            <KV label="Config file" value={s.backends.rados.config_file} />
            <KV label="User" value={s.backends.rados.user} />
            <KV label="Pool" value={s.backends.rados.pool} />
            <KV
              label="Namespace"
              value={s.backends.rados.namespace || <span className="text-muted-foreground">—</span>}
            />
            <KV
              label="Storage classes"
              value={s.backends.rados.classes || <span className="text-muted-foreground">—</span>}
            />
          </CardContent>
        </Card>
      )}

      {data === 's3' && (
        <>
          {dataBackendQuery.isLoading && <Skeleton className="h-56 w-full" />}
          {dataBackendQuery.data && dataBackendQuery.data.kind === 's3' && (
            <S3BackendCard s3={dataBackendQuery.data} />
          )}
        </>
      )}
    </div>
  );
}

export function SettingsPage() {
  const settingsQuery = useQuery({
    queryKey: queryKeys.settings.all,
    queryFn: fetchSettings,
    meta: { label: 'settings' },
  });

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Settings</h1>
        <p className="text-sm text-muted-foreground">
          Read-only cluster identity, console knobs, and per-backend connection
          parameters.
        </p>
      </div>
      {settingsQuery.isLoading && <Skeleton className="h-72 w-full" />}
      {settingsQuery.data && (
        <Tabs defaultValue="cluster" className="space-y-4">
          <TabsList>
            <TabsTrigger value="cluster">Cluster</TabsTrigger>
            <TabsTrigger value="console">Console</TabsTrigger>
            <TabsTrigger value="backends">Backends</TabsTrigger>
          </TabsList>
          <TabsContent value="cluster">
            <ClusterTab s={settingsQuery.data} />
          </TabsContent>
          <TabsContent value="console">
            <ConsoleTab s={settingsQuery.data} />
          </TabsContent>
          <TabsContent value="backends">
            <BackendsTab s={settingsQuery.data} />
          </TabsContent>
        </Tabs>
      )}
    </div>
  );
}
