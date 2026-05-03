import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, Loader2, Plus, Trash2 } from 'lucide-react';

import {
  fetchBucketACL,
  setBucketACL,
  type ACLCanned,
  type ACLConfig,
  type ACLGrant,
  type ACLGranteeType,
  type ACLPermission,
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';

const CANNED_OPTIONS: ReadonlyArray<{ value: ACLCanned; label: string; hint: string }> = [
  { value: 'private', label: 'private', hint: 'Owner gets FULL_CONTROL. No public access.' },
  { value: 'public-read', label: 'public-read', hint: 'AllUsers READ.' },
  {
    value: 'public-read-write',
    label: 'public-read-write',
    hint: 'AllUsers READ and WRITE — rarely correct.',
  },
  {
    value: 'authenticated-read',
    label: 'authenticated-read',
    hint: 'Any signed-in AWS account READ.',
  },
  {
    value: 'log-delivery-write',
    label: 'log-delivery-write',
    hint: 'Required on the target bucket for S3 access logging.',
  },
];

const PERMISSIONS: ReadonlyArray<ACLPermission> = [
  'FULL_CONTROL',
  'READ',
  'WRITE',
  'READ_ACP',
  'WRITE_ACP',
];

const GRANTEE_TYPES: ReadonlyArray<{ value: ACLGranteeType; label: string }> = [
  { value: 'CanonicalUser', label: 'CanonicalUser' },
  { value: 'Group', label: 'Group' },
  { value: 'AmazonCustomerByEmail', label: 'AmazonCustomerByEmail' },
];

// Canonical AWS group URIs the operator can pick from instead of typing the
// full path. Not exhaustive — matches the values the gateway accepts in
// internal/s3api/acl.go::allowedGroupURIs.
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

function emptyGrant(): ACLGrant {
  return { grantee_type: 'CanonicalUser', permission: 'READ' };
}

function grantsEqual(a: ACLGrant[], b: ACLGrant[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    const x = a[i];
    const y = b[i];
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

export function BucketACLTab({ bucket }: Props) {
  const aclQ = useQuery({
    queryKey: queryKeys.buckets.acl(bucket.name),
    queryFn: () => fetchBucketACL(bucket.name),
    meta: { silent: true },
  });

  const initial = useMemo<ACLConfig>(
    () => aclQ.data ?? { canned: 'private', grants: [] },
    [aclQ.data],
  );
  const [canned, setCanned] = useState<ACLCanned>(initial.canned);
  const [grants, setGrants] = useState<ACLGrant[]>(initial.grants);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    setCanned(initial.canned);
    setGrants(initial.grants);
    setServerError(null);
  }, [initial]);

  const dirty = canned !== initial.canned || !grantsEqual(grants, initial.grants);

  function updateGrant(idx: number, patch: Partial<ACLGrant>) {
    setGrants((prev) => prev.map((g, i) => (i === idx ? { ...g, ...patch } : g)));
  }

  function changeGranteeType(idx: number, gt: ACLGranteeType) {
    setGrants((prev) =>
      prev.map((g, i) => {
        if (i !== idx) return g;
        // Reset the type-specific identifier when switching grantee shape.
        return {
          grantee_type: gt,
          permission: g.permission,
          id: gt === 'CanonicalUser' ? (g.id ?? '') : undefined,
          uri: gt === 'Group' ? (g.uri ?? GROUP_URIS[0].uri) : undefined,
          email: gt === 'AmazonCustomerByEmail' ? (g.email ?? '') : undefined,
          display_name: gt === 'CanonicalUser' ? g.display_name : undefined,
        };
      }),
    );
  }

  function addGrant() {
    setGrants((prev) => [...prev, emptyGrant()]);
  }

  function removeGrant(idx: number) {
    setGrants((prev) => prev.filter((_, i) => i !== idx));
  }

  async function handleSave() {
    setSaving(true);
    setServerError(null);
    try {
      // Drop any client-side empty fields the Go validator would reject so
      // the operator gets the inline error before the network round-trip.
      for (const g of grants) {
        if (g.grantee_type === 'CanonicalUser' && !(g.id ?? '').trim()) {
          throw asAdminError('InvalidArgument', 'CanonicalUser grant requires an ID');
        }
        if (g.grantee_type === 'Group' && !(g.uri ?? '').trim()) {
          throw asAdminError('InvalidArgument', 'Group grant requires a URI');
        }
        if (g.grantee_type === 'AmazonCustomerByEmail' && !(g.email ?? '').trim()) {
          throw asAdminError('InvalidArgument', 'AmazonCustomerByEmail grant requires an email');
        }
      }
      await setBucketACL(bucket.name, { canned, grants });
      // Re-fetch to confirm round-trip per AC (Save reloads from GetBucketGrants).
      await queryClient.invalidateQueries({ queryKey: queryKeys.buckets.acl(bucket.name) });
      showToast({ title: 'ACL updated', description: bucket.name });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSaving(false);
    }
  }

  function handleReset() {
    setCanned(initial.canned);
    setGrants(initial.grants);
    setServerError(null);
  }

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Canned ACL</CardTitle>
          <CardDescription>
            Sets the bucket-wide preset. Custom grants below are persisted independently.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <fieldset className="grid gap-2 text-sm" disabled={saving}>
            {CANNED_OPTIONS.map((opt) => (
              <label
                key={opt.value}
                className="flex items-start gap-2 rounded-md border p-2 hover:bg-muted/30"
              >
                <input
                  type="radio"
                  name="bucket-canned-acl"
                  value={opt.value}
                  checked={canned === opt.value}
                  onChange={() => setCanned(opt.value)}
                  className="mt-0.5"
                />
                <div>
                  <div className="font-medium">{opt.label}</div>
                  <div className="text-xs text-muted-foreground">{opt.hint}</div>
                </div>
              </label>
            ))}
          </fieldset>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Explicit grants</CardTitle>
          <CardDescription>
            Per-grantee overrides applied alongside the canned ACL. Empty list resets to canned-only.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {grants.length === 0 && (
            <p className="text-sm text-muted-foreground">No grants — bucket uses the canned ACL only.</p>
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
                  <GrantIdentifierField grant={g} idx={idx} onPatch={updateGrant} />
                </div>
              </div>
              <div className="grid gap-2 sm:grid-cols-3">
                <div>
                  <Label className="text-xs">Permission</Label>
                  <Select
                    value={g.permission}
                    onValueChange={(v) => updateGrant(idx, { permission: v as ACLPermission })}
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
                      onChange={(e) => updateGrant(idx, { display_name: e.target.value })}
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
            <ErrorBanner code={serverError.code} message={serverError.message} />
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
            onClick={handleSave}
            disabled={!dirty || saving}
          >
            {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
            Save ACL
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
  grant: ACLGrant;
  idx: number;
  onPatch: (idx: number, patch: Partial<ACLGrant>) => void;
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

function asAdminError(code: string, message: string): AdminApiError {
  const err = new Error(message) as AdminApiError;
  err.code = code;
  err.status = 0;
  return err;
}

function ErrorBanner({ code, message }: { code: string; message: string }) {
  return (
    <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
      <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
      <div>
        <div className="font-medium">{code}</div>
        <div className="text-xs text-destructive/80">{message}</div>
      </div>
    </div>
  );
}
