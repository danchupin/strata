import { Suspense, lazy, useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, ArrowDown, ArrowUp, Loader2, Plus, Trash2 } from 'lucide-react';

import {
  fetchBucketLifecycle,
  setBucketLifecycle,
  type AdminApiError,
  type BucketDetail,
  type LifecycleConfig,
  type LifecycleRule,
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import schemaJson from '@/schemas/lifecycle.json';

// Heavyweight Monaco JSON editor — lazy-loaded so the Buckets / BucketDetail
// initial bundle stays under the 500 KiB gzipped budget. The whole monaco-
// editor module + its workers land in a separate vite chunk.
const MonacoJsonEditor = lazy(() => import('./MonacoJsonEditor'));

const STORAGE_CLASSES = [
  'STANDARD_IA',
  'ONEZONE_IA',
  'INTELLIGENT_TIERING',
  'GLACIER',
  'DEEP_ARCHIVE',
  'GLACIER_IR',
] as const;

interface Props {
  bucket: BucketDetail;
}

export function BucketLifecycleTab({ bucket }: Props) {
  const lcQ = useQuery({
    queryKey: queryKeys.buckets.lifecycle(bucket.name),
    queryFn: () => fetchBucketLifecycle(bucket.name),
    meta: { silent: true },
  });

  const initial = useMemo<LifecycleConfig>(
    () => lcQ.data ?? { rules: [] },
    [lcQ.data],
  );
  const [rules, setRules] = useState<LifecycleRule[]>(initial.rules);
  const [jsonText, setJsonText] = useState<string>(formatJson(initial));
  const [activeTab, setActiveTab] = useState<'visual' | 'json'>('visual');
  const [jsonError, setJsonError] = useState<string | null>(null);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );
  const [saving, setSaving] = useState(false);

  // Reset local state when the underlying server data changes (initial load
  // or after a successful save). Reset both the visual rule list and the
  // JSON tab buffer so they stay in lock-step until the operator edits.
  useEffect(() => {
    setRules(initial.rules);
    setJsonText(formatJson(initial));
    setJsonError(null);
    setServerError(null);
  }, [initial]);

  function syncJsonFromVisual(next: LifecycleRule[]) {
    setJsonText(formatJson({ rules: next }));
    setJsonError(null);
  }

  function applyJsonToVisual(): boolean {
    try {
      const parsed = JSON.parse(jsonText) as LifecycleConfig;
      if (!parsed || !Array.isArray(parsed.rules)) {
        setJsonError('Top-level "rules" array is required');
        return false;
      }
      setRules(parsed.rules);
      setJsonError(null);
      return true;
    } catch (err) {
      setJsonError((err as Error).message);
      return false;
    }
  }

  async function handleSave() {
    if (activeTab === 'json') {
      // Reparse JSON tab so the server payload reflects the latest text.
      if (!applyJsonToVisual()) return;
    }
    const next: LifecycleConfig = {
      rules: activeTab === 'json' ? safeParseRules(jsonText) ?? rules : rules,
    };
    if (next.rules.length === 0) {
      setServerError({ code: 'InvalidArgument', message: 'at least one rule is required' });
      return;
    }
    setSaving(true);
    setServerError(null);
    try {
      await setBucketLifecycle(bucket.name, next);
      showToast({ title: 'Lifecycle updated', description: bucket.name });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.lifecycle(bucket.name),
      });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSaving(false);
    }
  }

  function handleAddRule() {
    const next: LifecycleRule[] = [
      ...rules,
      {
        id: `rule-${rules.length + 1}`,
        status: 'Enabled',
        expiration: { days: 30 },
      },
    ];
    setRules(next);
    syncJsonFromVisual(next);
  }

  function handleUpdateRule(idx: number, updater: (r: LifecycleRule) => LifecycleRule) {
    const next = rules.map((r, i) => (i === idx ? updater(r) : r));
    setRules(next);
    syncJsonFromVisual(next);
  }

  function handleDeleteRule(idx: number) {
    const next = rules.filter((_, i) => i !== idx);
    setRules(next);
    syncJsonFromVisual(next);
  }

  function handleMoveRule(idx: number, dir: -1 | 1) {
    const target = idx + dir;
    if (target < 0 || target >= rules.length) return;
    const next = [...rules];
    [next[idx], next[target]] = [next[target], next[idx]];
    setRules(next);
    syncJsonFromVisual(next);
  }

  function handleTabChange(value: string) {
    if (value === 'visual' && activeTab === 'json') {
      // Switching out of the JSON tab applies edits back into the visual
      // model — confirm if there's an unsaved parse error per AC.
      if (jsonError) {
        const ok = window.confirm(
          'JSON contains unsaved errors. Discard JSON edits and revert to the visual state?',
        );
        if (!ok) return;
        setJsonText(formatJson({ rules }));
        setJsonError(null);
      } else {
        applyJsonToVisual();
      }
    }
    if (value === 'json' && activeTab === 'visual') {
      setJsonText(formatJson({ rules }));
    }
    setActiveTab(value as 'visual' | 'json');
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Lifecycle rules</CardTitle>
        <CardDescription>
          Per-bucket S3 lifecycle: transitions, expirations, noncurrent versions,
          and abort-incomplete-multipart cleanup.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <Tabs value={activeTab} onValueChange={handleTabChange}>
          <TabsList>
            <TabsTrigger value="visual">Visual</TabsTrigger>
            <TabsTrigger value="json">JSON</TabsTrigger>
          </TabsList>
          <TabsContent value="visual" className="space-y-3">
            {lcQ.isPending && (
              <p className="text-sm text-muted-foreground">Loading…</p>
            )}
            {!lcQ.isPending && rules.length === 0 && (
              <p className="text-sm text-muted-foreground">
                No lifecycle rules configured.
              </p>
            )}
            {rules.map((r, i) => (
              <RuleCard
                key={`${r.id}-${i}`}
                rule={r}
                onChange={(updater) => handleUpdateRule(i, updater)}
                onDelete={() => handleDeleteRule(i)}
                onMoveUp={i > 0 ? () => handleMoveRule(i, -1) : undefined}
                onMoveDown={i < rules.length - 1 ? () => handleMoveRule(i, 1) : undefined}
              />
            ))}
            <Button type="button" variant="outline" size="sm" onClick={handleAddRule}>
              <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden /> Add rule
            </Button>
          </TabsContent>
          <TabsContent value="json" className="space-y-2">
            <Suspense
              fallback={
                <p className="text-sm text-muted-foreground">Loading editor…</p>
              }
            >
              <MonacoJsonEditor
                value={jsonText}
                onChange={(next) => {
                  setJsonText(next);
                  setJsonError(null);
                }}
                schema={{
                  uri: 'https://strata.local/schemas/lifecycle.json',
                  modelUri: `inmemory://lifecycle/${bucket.name}.json`,
                  schema: schemaJson as object,
                }}
                height={460}
              />
            </Suspense>
            {jsonError && (
              <p className="text-xs text-destructive">{jsonError}</p>
            )}
          </TabsContent>
        </Tabs>
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
      <CardFooter className="justify-end">
        <Button
          type="button"
          size="sm"
          disabled={saving || lcQ.isPending}
          onClick={handleSave}
        >
          {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
          Save lifecycle
        </Button>
      </CardFooter>
    </Card>
  );
}

interface RuleCardProps {
  rule: LifecycleRule;
  onChange: (updater: (r: LifecycleRule) => LifecycleRule) => void;
  onDelete: () => void;
  onMoveUp?: () => void;
  onMoveDown?: () => void;
}

function RuleCard({ rule, onChange, onDelete, onMoveUp, onMoveDown }: RuleCardProps) {
  const filterPrefix = rule.filter?.prefix ?? '';
  const tags = rule.filter?.tags ?? [];

  function setFilter(next: Partial<NonNullable<LifecycleRule['filter']>>) {
    onChange((r) => {
      const merged = { ...(r.filter ?? {}), ...next };
      const keep =
        (merged.prefix && merged.prefix.length > 0) ||
        (merged.tags && merged.tags.length > 0);
      return { ...r, filter: keep ? merged : undefined };
    });
  }

  function setExpiration(next: Partial<NonNullable<LifecycleRule['expiration']>> | null) {
    onChange((r) => ({ ...r, expiration: next === null ? undefined : { ...(r.expiration ?? {}), ...next } }));
  }

  function setTransition(idx: number, patch: Partial<{ days: number; date: string; storage_class: string }>) {
    onChange((r) => {
      const list = [...(r.transitions ?? [])];
      list[idx] = { ...list[idx], ...patch };
      return { ...r, transitions: list };
    });
  }

  function addTransition() {
    onChange((r) => ({
      ...r,
      transitions: [...(r.transitions ?? []), { days: 30, storage_class: 'STANDARD_IA' }],
    }));
  }

  function removeTransition(idx: number) {
    onChange((r) => ({
      ...r,
      transitions: (r.transitions ?? []).filter((_, i) => i !== idx),
    }));
  }

  function setNoncurrentExpiration(days: number | null) {
    onChange((r) => ({
      ...r,
      noncurrent_version_expiration:
        days === null || Number.isNaN(days) ? undefined : { noncurrent_days: days },
    }));
  }

  function setAbort(days: number | null) {
    onChange((r) => ({
      ...r,
      abort_incomplete_multipart_upload:
        days === null || Number.isNaN(days) ? undefined : { days_after_initiation: days },
    }));
  }

  return (
    <div className="space-y-3 rounded-md border p-3">
      <div className="flex flex-wrap items-end gap-3">
        <div className="grow space-y-1">
          <Label htmlFor={`rule-id-${rule.id}`}>ID</Label>
          <Input
            id={`rule-id-${rule.id}`}
            value={rule.id}
            onChange={(e) => onChange((r) => ({ ...r, id: e.target.value }))}
            className="h-9"
          />
        </div>
        <div className="space-y-1">
          <Label htmlFor={`rule-status-${rule.id}`}>Status</Label>
          <select
            id={`rule-status-${rule.id}`}
            className="block h-9 rounded-md border border-input bg-background px-2 text-sm"
            value={rule.status}
            onChange={(e) => onChange((r) => ({ ...r, status: e.target.value as LifecycleRule['status'] }))}
          >
            <option value="Enabled">Enabled</option>
            <option value="Disabled">Disabled</option>
          </select>
        </div>
        <div className="ml-auto flex items-center gap-1">
          <Button
            type="button"
            variant="outline"
            size="icon"
            className="h-8 w-8"
            disabled={!onMoveUp}
            onClick={onMoveUp}
            aria-label="Move rule up"
          >
            <ArrowUp className="h-3.5 w-3.5" aria-hidden />
          </Button>
          <Button
            type="button"
            variant="outline"
            size="icon"
            className="h-8 w-8"
            disabled={!onMoveDown}
            onClick={onMoveDown}
            aria-label="Move rule down"
          >
            <ArrowDown className="h-3.5 w-3.5" aria-hidden />
          </Button>
          <Button
            type="button"
            variant="outline"
            size="icon"
            className="h-8 w-8 text-destructive"
            onClick={onDelete}
            aria-label="Delete rule"
          >
            <Trash2 className="h-3.5 w-3.5" aria-hidden />
          </Button>
        </div>
      </div>

      <fieldset className="space-y-2">
        <legend className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Filter
        </legend>
        <div className="grid gap-2 sm:grid-cols-2">
          <div className="space-y-1">
            <Label htmlFor={`rule-prefix-${rule.id}`}>Prefix</Label>
            <Input
              id={`rule-prefix-${rule.id}`}
              value={filterPrefix}
              onChange={(e) => setFilter({ prefix: e.target.value })}
              className="h-9"
              placeholder="logs/"
            />
          </div>
          <div className="space-y-1">
            <Label>Tags</Label>
            <div className="space-y-1">
              {tags.map((t, i) => (
                <div key={i} className="flex items-center gap-1">
                  <Input
                    value={t.key}
                    placeholder="key"
                    className="h-8"
                    onChange={(e) =>
                      setFilter({
                        tags: tags.map((tt, j) => (j === i ? { ...tt, key: e.target.value } : tt)),
                      })
                    }
                  />
                  <Input
                    value={t.value}
                    placeholder="value"
                    className="h-8"
                    onChange={(e) =>
                      setFilter({
                        tags: tags.map((tt, j) => (j === i ? { ...tt, value: e.target.value } : tt)),
                      })
                    }
                  />
                  <Button
                    type="button"
                    variant="outline"
                    size="icon"
                    className="h-8 w-8"
                    onClick={() => setFilter({ tags: tags.filter((_, j) => j !== i) })}
                    aria-label="Remove tag"
                  >
                    <Trash2 className="h-3.5 w-3.5" aria-hidden />
                  </Button>
                </div>
              ))}
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setFilter({ tags: [...tags, { key: '', value: '' }] })}
              >
                <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden /> Add tag
              </Button>
            </div>
          </div>
        </div>
      </fieldset>

      <fieldset className="space-y-2">
        <legend className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Expiration
        </legend>
        <div className="grid gap-2 sm:grid-cols-2">
          <div className="space-y-1">
            <Label htmlFor={`exp-days-${rule.id}`}>Days</Label>
            <Input
              id={`exp-days-${rule.id}`}
              type="number"
              min={0}
              value={rule.expiration?.days ?? ''}
              onChange={(e) =>
                setExpiration({
                  days: e.target.value ? Number(e.target.value) : 0,
                  date: '',
                })
              }
              className="h-9"
            />
          </div>
          <div className="space-y-1">
            <Label htmlFor={`exp-date-${rule.id}`}>Date (ISO 8601)</Label>
            <Input
              id={`exp-date-${rule.id}`}
              type="text"
              placeholder="2030-01-01T00:00:00Z"
              value={rule.expiration?.date ?? ''}
              onChange={(e) => setExpiration({ date: e.target.value, days: 0 })}
              className="h-9"
            />
          </div>
        </div>
        <Button type="button" variant="outline" size="sm" onClick={() => setExpiration(null)}>
          Clear expiration
        </Button>
      </fieldset>

      <fieldset className="space-y-2">
        <legend className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Transitions
        </legend>
        {(rule.transitions ?? []).length === 0 && (
          <p className="text-xs text-muted-foreground">No transitions.</p>
        )}
        {(rule.transitions ?? []).map((t, i) => (
          <div key={i} className="grid gap-2 sm:grid-cols-[1fr_1fr_1fr_auto]">
            <div className="space-y-1">
              <Label className="text-xs">Days</Label>
              <Input
                type="number"
                min={0}
                value={t.days ?? ''}
                onChange={(e) => setTransition(i, { days: e.target.value ? Number(e.target.value) : 0, date: '' })}
                className="h-9"
              />
            </div>
            <div className="space-y-1">
              <Label className="text-xs">Date</Label>
              <Input
                type="text"
                placeholder="2030-01-01T00:00:00Z"
                value={t.date ?? ''}
                onChange={(e) => setTransition(i, { date: e.target.value, days: 0 })}
                className="h-9"
              />
            </div>
            <div className="space-y-1">
              <Label className="text-xs">Storage class</Label>
              <select
                className="block h-9 w-full rounded-md border border-input bg-background px-2 text-sm"
                value={t.storage_class}
                onChange={(e) => setTransition(i, { storage_class: e.target.value })}
              >
                {STORAGE_CLASSES.map((sc) => (
                  <option key={sc} value={sc}>
                    {sc}
                  </option>
                ))}
              </select>
            </div>
            <div className="flex items-end">
              <Button
                type="button"
                variant="outline"
                size="icon"
                className="h-9 w-9"
                onClick={() => removeTransition(i)}
                aria-label="Remove transition"
              >
                <Trash2 className="h-3.5 w-3.5" aria-hidden />
              </Button>
            </div>
          </div>
        ))}
        <Button type="button" variant="outline" size="sm" onClick={addTransition}>
          <Plus className="mr-1.5 h-3.5 w-3.5" aria-hidden /> Add transition
        </Button>
      </fieldset>

      <fieldset className="space-y-2">
        <legend className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Noncurrent version expiration
        </legend>
        <div className="space-y-1">
          <Label htmlFor={`nce-${rule.id}`}>Noncurrent days</Label>
          <Input
            id={`nce-${rule.id}`}
            type="number"
            min={0}
            value={rule.noncurrent_version_expiration?.noncurrent_days ?? ''}
            onChange={(e) => setNoncurrentExpiration(e.target.value ? Number(e.target.value) : null)}
            className="h-9 max-w-[180px]"
          />
        </div>
      </fieldset>

      <fieldset className="space-y-2">
        <legend className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Abort incomplete multipart
        </legend>
        <div className="space-y-1">
          <Label htmlFor={`abort-${rule.id}`}>Days after initiation</Label>
          <Input
            id={`abort-${rule.id}`}
            type="number"
            min={0}
            value={rule.abort_incomplete_multipart_upload?.days_after_initiation ?? ''}
            onChange={(e) => setAbort(e.target.value ? Number(e.target.value) : null)}
            className="h-9 max-w-[180px]"
          />
        </div>
      </fieldset>
    </div>
  );
}

function formatJson(cfg: LifecycleConfig): string {
  return JSON.stringify(cfg, null, 2);
}

function safeParseRules(text: string): LifecycleRule[] | null {
  try {
    const parsed = JSON.parse(text) as LifecycleConfig;
    if (parsed && Array.isArray(parsed.rules)) return parsed.rules;
    return null;
  } catch {
    return null;
  }
}
