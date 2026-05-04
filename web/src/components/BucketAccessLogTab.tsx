import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, Loader2, Plus, Trash2 } from 'lucide-react';

import {
  deleteBucketLogging,
  fetchBucketLogging,
  fetchBucketsList,
  setBucketLogging,
  type ACLGranteeType,
  type AdminApiError,
  type BucketDetail,
  type LoggingConfig,
  type LoggingGrant,
  type LoggingPermission,
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

const PERMISSIONS: ReadonlyArray<LoggingPermission> = ['FULL_CONTROL', 'READ', 'WRITE'];

const GRANTEE_TYPES: ReadonlyArray<{ value: ACLGranteeType; label: string }> = [
  { value: 'CanonicalUser', label: 'CanonicalUser' },
  { value: 'Group', label: 'Group' },
  { value: 'AmazonCustomerByEmail', label: 'AmazonCustomerByEmail' },
];

const GROUP_URIS: ReadonlyArray<{ name: string; uri: string }> = [
  { name: 'AllUsers', uri: 'http://acs.amazonaws.com/groups/global/AllUsers' },
  {
    name: 'AuthenticatedUsers',
    uri: 'http://acs.amazonaws.com/groups/global/AuthenticatedUsers',
  },
  { name: 'LogDelivery', uri: 'http://acs.amazonaws.com/groups/s3/LogDelivery' },
];

interface Props {
  bucket: BucketDetail;
}

function emptyGrant(): LoggingGrant {
  return { grantee_type: 'CanonicalUser', permission: 'READ' };
}

function emptyConfig(): LoggingConfig {
  return { target_bucket: '', target_prefix: '', target_grants: [] };
}

function configsEqual(a: LoggingConfig, b: LoggingConfig): boolean {
  if (a.target_bucket !== b.target_bucket) return false;
  if (a.target_prefix !== b.target_prefix) return false;
  const ag = a.target_grants ?? [];
  const bg = b.target_grants ?? [];
  if (ag.length !== bg.length) return false;
  for (let i = 0; i < ag.length; i++) {
    const x = ag[i];
    const y = bg[i];
    if (
      x.grantee_type !== y.grantee_type ||
      (x.id ?? '') !== (y.id ?? '') ||
      (x.uri ?? '') !== (y.uri ?? '') ||
      (x.display_name ?? '') !== (y.display_name ?? '') ||
      (x.email ?? '') !== (y.email ?? '') ||
      x.permission !== y.permission
    ) {
      return false;
    }
  }
  return true;
}

export function BucketAccessLogTab({ bucket }: Props) {
  const loggingQ = useQuery({
    queryKey: queryKeys.buckets.logging(bucket.name),
    queryFn: () => fetchBucketLogging(bucket.name),
    meta: { silent: true },
  });

  const initial = useMemo<LoggingConfig>(
    () => loggingQ.data ?? emptyConfig(),
    [loggingQ.data],
  );
  const isConfigured = loggingQ.data != null;

  const [cfg, setCfg] = useState<LoggingConfig>(initial);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );
  const [saving, setSaving] = useState(false);
  const [disabling, setDisabling] = useState(false);

  useEffect(() => {
    setCfg(initial);
    setServerError(null);
  }, [initial]);

  const bucketsQ = useQuery({
    queryKey: ['buckets', 'list', { query: '', sort: '', order: 'asc', page: 1, pageSize: 1000 }],
    queryFn: () => fetchBucketsList({ pageSize: 1000 }),
    meta: { silent: true },
  });
  const bucketSuggestions = useMemo(
    () => (bucketsQ.data?.buckets ?? []).map((b) => b.name).sort(),
    [bucketsQ.data],
  );

  const dirty = !configsEqual(cfg, initial);
  const grants = cfg.target_grants ?? [];

  function patch(p: Partial<LoggingConfig>) {
    setCfg((prev) => ({ ...prev, ...p }));
  }

  function patchGrant(idx: number, p: Partial<LoggingGrant>) {
    setCfg((prev) => ({
      ...prev,
      target_grants: (prev.target_grants ?? []).map((g, i) =>
        i === idx ? { ...g, ...p } : g,
      ),
    }));
  }

  function changeGranteeType(idx: number, gt: ACLGranteeType) {
    setCfg((prev) => ({
      ...prev,
      target_grants: (prev.target_grants ?? []).map((g, i) => {
        if (i !== idx) return g;
        return {
          grantee_type: gt,
          permission: g.permission,
          id: gt === 'CanonicalUser' ? (g.id ?? '') : undefined,
          uri: gt === 'Group' ? (g.uri ?? GROUP_URIS[0].uri) : undefined,
          email: gt === 'AmazonCustomerByEmail' ? (g.email ?? '') : undefined,
          display_name: gt === 'CanonicalUser' ? g.display_name : undefined,
        };
      }),
    }));
  }

  function addGrant() {
    setCfg((prev) => ({
      ...prev,
      target_grants: [...(prev.target_grants ?? []), emptyGrant()],
    }));
  }

  function removeGrant(idx: number) {
    setCfg((prev) => ({
      ...prev,
      target_grants: (prev.target_grants ?? []).filter((_, i) => i !== idx),
    }));
  }

  function handleReset() {
    setCfg(initial);
    setServerError(null);
  }

  async function handleSave() {
    setServerError(null);
    if (!cfg.target_bucket.trim()) {
      setServerError({
        code: 'InvalidArgument',
        message: 'Target bucket is required',
      });
      return;
    }
    for (let i = 0; i < grants.length; i++) {
      const g = grants[i];
      if (g.grantee_type === 'CanonicalUser' && !(g.id ?? '').trim()) {
        setServerError({
          code: 'InvalidArgument',
          message: `target_grants[${i}]: CanonicalUser grant requires an ID`,
        });
        return;
      }
      if (g.grantee_type === 'Group' && !(g.uri ?? '').trim()) {
        setServerError({
          code: 'InvalidArgument',
          message: `target_grants[${i}]: Group grant requires a URI`,
        });
        return;
      }
      if (g.grantee_type === 'AmazonCustomerByEmail' && !(g.email ?? '').trim()) {
        setServerError({
          code: 'InvalidArgument',
          message: `target_grants[${i}]: AmazonCustomerByEmail grant requires an email`,
        });
        return;
      }
    }
    setSaving(true);
    try {
      const payload: LoggingConfig = {
        target_bucket: cfg.target_bucket.trim(),
        target_prefix: cfg.target_prefix,
        target_grants: grants.length > 0 ? grants : undefined,
      };
      await setBucketLogging(bucket.name, payload);
      await queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.logging(bucket.name),
      });
      showToast({ title: 'Access log updated', description: bucket.name });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSaving(false);
    }
  }

  async function handleDisable() {
    if (!window.confirm(`Disable access logging on ${bucket.name}?`)) return;
    setDisabling(true);
    setServerError(null);
    try {
      await deleteBucketLogging(bucket.name);
      await queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.logging(bucket.name),
      });
      showToast({ title: 'Access logging disabled', description: bucket.name });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setDisabling(false);
    }
  }

  // Inline preview per AC: 'Logs will land at s3://<target-bucket>/<prefix>YYYY-MM-DD-HH-MM-SS-RAND/'.
  const preview = useMemo(() => {
    const tb = cfg.target_bucket.trim() || '<target-bucket>';
    const tp = cfg.target_prefix ?? '';
    return `s3://${tb}/${tp}YYYY-MM-DD-HH-MM-SS-RAND/`;
  }, [cfg.target_bucket, cfg.target_prefix]);

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle className="text-base">Access log target</CardTitle>
            <CardDescription>
              Stream HTTP access logs from this bucket into a target bucket. The target bucket
              must grant <code>log-delivery-write</code> ACL or an explicit grant to the
              <code> LogDelivery</code> group.
            </CardDescription>
          </div>
          {isConfigured && (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => void handleDisable()}
              disabled={disabling || saving}
            >
              {disabling && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
              Disable logging
            </Button>
          )}
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <div>
              <Label className="text-xs" htmlFor="log-target-bucket">
                Target bucket
              </Label>
              <Input
                id="log-target-bucket"
                value={cfg.target_bucket}
                onChange={(e) => patch({ target_bucket: e.target.value })}
                placeholder="logs-bucket"
                list="log-target-buckets"
                disabled={saving}
              />
              <datalist id="log-target-buckets">
                {bucketSuggestions.map((b) => (
                  <option key={b} value={b} />
                ))}
              </datalist>
            </div>
            <div>
              <Label className="text-xs" htmlFor="log-target-prefix">
                Target prefix
              </Label>
              <Input
                id="log-target-prefix"
                value={cfg.target_prefix}
                onChange={(e) => patch({ target_prefix: e.target.value })}
                placeholder="access/"
                disabled={saving}
              />
            </div>
          </div>
          <div className="rounded-md border bg-muted/30 px-3 py-2 text-xs">
            <span className="text-muted-foreground">Logs will land at: </span>
            <code className="break-all font-mono">{preview}</code>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Target grants</CardTitle>
          <CardDescription>
            Optional explicit grants on each delivered log object (in addition to the target
            bucket's ACL). Permissions: FULL_CONTROL | READ | WRITE.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {grants.length === 0 && (
            <p className="text-sm text-muted-foreground">
              No grants — the target bucket's canned ACL applies.
            </p>
          )}
          {grants.map((g, idx) => (
            <div key={idx} className="grid gap-2 rounded-md border p-3">
              <div className="grid gap-2 sm:grid-cols-3">
                <div>
                  <Label className="text-xs">Grantee type</Label>
                  <Select
                    value={g.grantee_type}
                    onValueChange={(v) => changeGranteeType(idx, v as ACLGranteeType)}
                  >
                    <SelectTrigger className="h-9">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {GRANTEE_TYPES.map((t) => (
                        <SelectItem key={t.value} value={t.value}>
                          {t.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="sm:col-span-2">
                  <GrantIdentifierField grant={g} idx={idx} onPatch={patchGrant} />
                </div>
              </div>
              <div className="grid gap-2 sm:grid-cols-3">
                <div>
                  <Label className="text-xs">Permission</Label>
                  <Select
                    value={g.permission}
                    onValueChange={(v) => patchGrant(idx, { permission: v as LoggingPermission })}
                  >
                    <SelectTrigger className="h-9">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {PERMISSIONS.map((p) => (
                        <SelectItem key={p} value={p}>
                          {p}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                {g.grantee_type === 'CanonicalUser' && (
                  <div className="sm:col-span-2">
                    <Label className="text-xs">Display name (optional)</Label>
                    <Input
                      className="h-9"
                      value={g.display_name ?? ''}
                      onChange={(e) => patchGrant(idx, { display_name: e.target.value })}
                    />
                  </div>
                )}
                <div className="flex items-end justify-end sm:col-start-3">
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={() => removeGrant(idx)}
                    disabled={saving}
                  >
                    <Trash2 className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                    Remove
                  </Button>
                </div>
              </div>
            </div>
          ))}
          <Button type="button" variant="outline" size="sm" onClick={addGrant} disabled={saving}>
            <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden />
            Add grant
          </Button>
        </CardContent>
        {serverError && (
          <CardContent className="pt-0">
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <div>
                <div className="font-medium">{serverError.code}</div>
                <div className="text-xs text-destructive/80">{serverError.message}</div>
              </div>
            </div>
          </CardContent>
        )}
        <CardFooter className="justify-end gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={handleReset}
            disabled={!dirty || saving}
          >
            Reset
          </Button>
          <Button
            type="button"
            size="sm"
            onClick={() => void handleSave()}
            disabled={!dirty || saving}
          >
            {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
            Save
          </Button>
        </CardFooter>
      </Card>
    </div>
  );
}

function GrantIdentifierField({
  grant,
  idx,
  onPatch,
}: {
  grant: LoggingGrant;
  idx: number;
  onPatch: (idx: number, patch: Partial<LoggingGrant>) => void;
}) {
  if (grant.grantee_type === 'CanonicalUser') {
    return (
      <div>
        <Label className="text-xs">Canonical user ID</Label>
        <Input
          className="h-9"
          placeholder="strata-canonical-id"
          value={grant.id ?? ''}
          onChange={(e) => onPatch(idx, { id: e.target.value })}
        />
      </div>
    );
  }
  if (grant.grantee_type === 'Group') {
    const matchedPreset = GROUP_URIS.find((g) => g.uri === grant.uri);
    return (
      <div>
        <Label className="text-xs">Group</Label>
        <Select
          value={matchedPreset ? matchedPreset.uri : 'custom'}
          onValueChange={(v) => onPatch(idx, { uri: v === 'custom' ? grant.uri ?? '' : v })}
        >
          <SelectTrigger className="h-9">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {GROUP_URIS.map((g) => (
              <SelectItem key={g.uri} value={g.uri}>
                {g.name}
              </SelectItem>
            ))}
            <SelectItem value="custom">Custom URI…</SelectItem>
          </SelectContent>
        </Select>
        {!matchedPreset && (
          <Input
            className="mt-1 h-9"
            placeholder="http://acs.amazonaws.com/groups/..."
            value={grant.uri ?? ''}
            onChange={(e) => onPatch(idx, { uri: e.target.value })}
          />
        )}
      </div>
    );
  }
  return (
    <div>
      <Label className="text-xs">Email address</Label>
      <Input
        className="h-9"
        type="email"
        placeholder="user@example.com"
        value={grant.email ?? ''}
        onChange={(e) => onPatch(idx, { email: e.target.value })}
      />
    </div>
  );
}
