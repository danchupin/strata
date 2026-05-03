import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, Info, Loader2 } from 'lucide-react';

import {
  fetchBucketObjectLock,
  setBucketObjectLock,
  setBucketVersioning,
  type AdminApiError,
  type BucketDetail,
  type ObjectLockConfig,
  type ObjectLockMode,
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
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import { cn } from '@/lib/utils';

interface Props {
  bucket: BucketDetail;
}

type VersioningState = 'Enabled' | 'Suspended';
type RetentionMode = 'OFF' | 'GOVERNANCE' | 'COMPLIANCE';
type RetentionUnit = 'days' | 'years';

export function BucketOverviewTab({ bucket }: Props) {
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <VersioningCard bucket={bucket} />
      <ObjectLockCard bucket={bucket} />
    </div>
  );
}

function VersioningCard({ bucket }: { bucket: BucketDetail }) {
  // The detail's versioning label is one of "Enabled" | "Suspended" | "Off".
  // The card only writes Enabled or Suspended; Off is the freshly-created
  // baseline and the server rejects any attempt to flip back to it.
  const initial: VersioningState =
    bucket.versioning === 'Enabled' ? 'Enabled' : 'Suspended';
  const [state, setState] = useState<VersioningState>(initial);
  const [saving, setSaving] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );
  const dirty = state !== initial;

  useEffect(() => {
    setState(initial);
    setServerError(null);
  }, [initial]);

  async function handleSave() {
    setSaving(true);
    setServerError(null);
    try {
      await setBucketVersioning(bucket.name, state);
      void queryClient.invalidateQueries({ queryKey: queryKeys.buckets.one(bucket.name) });
      showToast({ title: `Versioning · ${state}`, description: bucket.name });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Versioning</CardTitle>
        <CardDescription>
          Control whether the bucket keeps prior versions of an object.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <fieldset className="space-y-2" disabled={saving}>
          <div className="flex items-center gap-4 text-sm">
            <label className="inline-flex items-center gap-1.5">
              <input
                type="radio"
                name="bucket-versioning"
                value="Enabled"
                checked={state === 'Enabled'}
                onChange={() => setState('Enabled')}
              />
              Enabled
            </label>
            <label className="inline-flex items-center gap-1.5">
              <input
                type="radio"
                name="bucket-versioning"
                value="Suspended"
                checked={state === 'Suspended'}
                onChange={() => setState('Suspended')}
              />
              Suspended
            </label>
          </div>
        </fieldset>
        {state === 'Enabled' && initial !== 'Enabled' && (
          <p className="inline-flex items-start gap-2 text-xs text-muted-foreground">
            <Info className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            Once enabled, versioning cannot be disabled, only suspended.
          </p>
        )}
        {serverError && (
          <ErrorBanner code={serverError.code} message={serverError.message} />
        )}
      </CardContent>
      <CardFooter className="justify-end gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={!dirty || saving}
          onClick={() => setState(initial)}
        >
          Reset
        </Button>
        <Button
          type="button"
          size="sm"
          disabled={!dirty || saving}
          onClick={handleSave}
        >
          {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
          Save versioning
        </Button>
      </CardFooter>
    </Card>
  );
}

function ObjectLockCard({ bucket }: { bucket: BucketDetail }) {
  // The card is greyed-out when:
  //  1. Versioning is suspended (S3 semantics — Object-Lock requires
  //     Versioning=Enabled); we surface the AC's exact tooltip.
  //  2. The bucket itself was not created with Object-Lock — the operator
  //     cannot enable a default retention rule retroactively (S3 semantics);
  //     Object-Lock has to be set at bucket creation time.
  const versioningSuspended = bucket.versioning !== 'Enabled';
  const lockBucketDisabled = !bucket.object_lock;
  const disabled = versioningSuspended || lockBucketDisabled;
  const tooltip = versioningSuspended
    ? 'Object-Lock requires Versioning=Enabled'
    : lockBucketDisabled
      ? 'Object-Lock must be enabled at bucket creation'
      : undefined;

  const cfgQ = useQuery({
    queryKey: queryKeys.buckets.objectLock(bucket.name),
    queryFn: () => fetchBucketObjectLock(bucket.name),
    enabled: !lockBucketDisabled,
    meta: { silent: true },
  });

  const initialRule = useMemo(() => deriveRuleState(cfgQ.data), [cfgQ.data]);
  const [mode, setMode] = useState<RetentionMode>(initialRule.mode);
  const [unit, setUnit] = useState<RetentionUnit>(initialRule.unit);
  const [amount, setAmount] = useState<string>(initialRule.amount);
  const [saving, setSaving] = useState(false);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );

  useEffect(() => {
    setMode(initialRule.mode);
    setUnit(initialRule.unit);
    setAmount(initialRule.amount);
    setServerError(null);
  }, [initialRule.mode, initialRule.unit, initialRule.amount]);

  const dirty =
    mode !== initialRule.mode ||
    (mode !== 'OFF' &&
      (unit !== initialRule.unit || amount !== initialRule.amount));

  const numAmount = Number(amount);
  const amountInvalid =
    mode !== 'OFF' && (!Number.isInteger(numAmount) || numAmount <= 0);

  async function handleSave() {
    if (disabled) return;
    setSaving(true);
    setServerError(null);
    const cfg: ObjectLockConfig = { object_lock_enabled: 'Enabled' };
    if (mode !== 'OFF') {
      const dr: ObjectLockConfig['rule'] = {
        default_retention: {
          mode: mode as ObjectLockMode,
          ...(unit === 'days' ? { days: numAmount } : { years: numAmount }),
        },
      };
      cfg.rule = dr;
    }
    try {
      await setBucketObjectLock(bucket.name, cfg);
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.objectLock(bucket.name),
      });
      void queryClient.invalidateQueries({ queryKey: queryKeys.buckets.one(bucket.name) });
      showToast({
        title: 'Object-Lock default updated',
        description: bucket.name,
      });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card className={cn(disabled && 'opacity-60')}>
      <CardHeader>
        <CardTitle className="text-base">Object-Lock default</CardTitle>
        <CardDescription>
          Default retention applied to new objects under WORM lock.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <fieldset
          className="space-y-2"
          disabled={disabled || saving || cfgQ.isPending}
          title={tooltip}
        >
          <div className="space-y-1.5">
            <Label htmlFor="ol-mode">Mode</Label>
            <select
              id="ol-mode"
              className="block h-9 w-full rounded-md border border-input bg-background px-2 text-sm"
              value={mode}
              onChange={(e) => setMode(e.target.value as RetentionMode)}
            >
              <option value="OFF">Off</option>
              <option value="GOVERNANCE">Governance</option>
              <option value="COMPLIANCE">Compliance</option>
            </select>
          </div>
          {mode !== 'OFF' && (
            <div className="grid grid-cols-2 gap-2">
              <div className="space-y-1.5">
                <Label htmlFor="ol-amount">Retention</Label>
                <Input
                  id="ol-amount"
                  type="number"
                  min={1}
                  step={1}
                  value={amount}
                  onChange={(e) => setAmount(e.target.value)}
                  aria-invalid={amountInvalid ? 'true' : 'false'}
                />
                {amountInvalid && (
                  <p className="text-xs text-destructive">must be a positive integer</p>
                )}
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="ol-unit">Unit</Label>
                <select
                  id="ol-unit"
                  className="block h-9 w-full rounded-md border border-input bg-background px-2 text-sm"
                  value={unit}
                  onChange={(e) => setUnit(e.target.value as RetentionUnit)}
                >
                  <option value="days">Days</option>
                  <option value="years">Years</option>
                </select>
              </div>
            </div>
          )}
        </fieldset>
        {tooltip && (
          <p className="inline-flex items-start gap-2 text-xs text-muted-foreground">
            <Info className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            {tooltip}
          </p>
        )}
        {serverError && (
          <ErrorBanner code={serverError.code} message={serverError.message} />
        )}
      </CardContent>
      <CardFooter className="justify-end gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={!dirty || saving || disabled}
          onClick={() => {
            setMode(initialRule.mode);
            setUnit(initialRule.unit);
            setAmount(initialRule.amount);
          }}
        >
          Reset
        </Button>
        <Button
          type="button"
          size="sm"
          disabled={disabled || !dirty || saving || amountInvalid}
          onClick={handleSave}
        >
          {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
          Save default
        </Button>
      </CardFooter>
    </Card>
  );
}

function deriveRuleState(cfg: ObjectLockConfig | undefined): {
  mode: RetentionMode;
  unit: RetentionUnit;
  amount: string;
} {
  const dr = cfg?.rule?.default_retention;
  if (!dr || !dr.mode) {
    return { mode: 'OFF', unit: 'days', amount: '30' };
  }
  if (dr.years && dr.years > 0) {
    return { mode: dr.mode, unit: 'years', amount: String(dr.years) };
  }
  if (dr.days && dr.days > 0) {
    return { mode: dr.mode, unit: 'days', amount: String(dr.days) };
  }
  return { mode: dr.mode, unit: 'days', amount: '30' };
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
