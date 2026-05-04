import { Suspense, lazy, useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AlertCircle, Loader2, Plus, Trash2 } from 'lucide-react';

import {
  deleteBucketCORS,
  fetchBucketCORS,
  setBucketCORS,
  type AdminApiError,
  type BucketDetail,
  type CORSConfig,
  type CORSRule,
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
import schemaJson from '@/schemas/cors.json';

// Lazy-loaded — reuses the existing Monaco lazy chunk shared with the
// Lifecycle/Policy editors so the per-page bundle stays under budget.
const MonacoJsonEditor = lazy(() => import('./MonacoJsonEditor'));

const HTTP_METHODS = ['GET', 'PUT', 'POST', 'DELETE', 'HEAD'] as const;
type HttpMethod = (typeof HTTP_METHODS)[number];

interface Props {
  bucket: BucketDetail;
}

export function BucketCORSTab({ bucket }: Props) {
  const corsQ = useQuery({
    queryKey: queryKeys.buckets.cors(bucket.name),
    queryFn: () => fetchBucketCORS(bucket.name),
    meta: { silent: true },
  });

  const initial = useMemo<CORSConfig>(
    () => corsQ.data ?? { rules: [] },
    [corsQ.data],
  );
  const [rules, setRules] = useState<CORSRule[]>(initial.rules);
  const [jsonText, setJsonText] = useState<string>(formatJson(initial));
  const [activeTab, setActiveTab] = useState<'visual' | 'json'>('visual');
  const [jsonError, setJsonError] = useState<string | null>(null);
  const [serverError, setServerError] = useState<{ code: string; message: string } | null>(
    null,
  );
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    setRules(initial.rules);
    setJsonText(formatJson(initial));
    setJsonError(null);
    setServerError(null);
  }, [initial]);

  function syncJsonFromVisual(next: CORSRule[]) {
    setJsonText(formatJson({ rules: next }));
    setJsonError(null);
  }

  function applyJsonToVisual(): boolean {
    try {
      const parsed = JSON.parse(jsonText) as CORSConfig;
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
      if (!applyJsonToVisual()) return;
    }
    const next: CORSConfig = {
      rules: activeTab === 'json' ? safeParseRules(jsonText) ?? rules : rules,
    };
    if (next.rules.length === 0) {
      setServerError({ code: 'InvalidArgument', message: 'at least one rule is required' });
      return;
    }
    setSaving(true);
    setServerError(null);
    try {
      await setBucketCORS(bucket.name, next);
      showToast({ title: 'CORS updated', description: bucket.name });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.cors(bucket.name),
      });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setSaving(false);
    }
  }

  async function handleDelete() {
    const ok = window.confirm(
      `Delete CORS configuration on "${bucket.name}"? This cannot be undone.`,
    );
    if (!ok) return;
    setDeleting(true);
    setServerError(null);
    try {
      await deleteBucketCORS(bucket.name);
      showToast({ title: 'CORS configuration deleted', description: bucket.name });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.buckets.cors(bucket.name),
      });
    } catch (err) {
      const e = err as AdminApiError;
      setServerError({ code: e.code ?? 'Error', message: e.message });
    } finally {
      setDeleting(false);
    }
  }

  function handleAddRule() {
    const next: CORSRule[] = [
      ...rules,
      {
        id: `rule-${rules.length + 1}`,
        allowed_methods: ['GET'],
        allowed_origins: ['*'],
      },
    ];
    setRules(next);
    syncJsonFromVisual(next);
  }

  function handleUpdateRule(idx: number, updater: (r: CORSRule) => CORSRule) {
    const next = rules.map((r, i) => (i === idx ? updater(r) : r));
    setRules(next);
    syncJsonFromVisual(next);
  }

  function handleDeleteRule(idx: number) {
    const next = rules.filter((_, i) => i !== idx);
    setRules(next);
    syncJsonFromVisual(next);
  }

  function handleTabChange(value: string) {
    if (value === 'visual' && activeTab === 'json') {
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
        <CardTitle className="text-base">CORS rules</CardTitle>
        <CardDescription>
          Per-bucket Cross-Origin Resource Sharing rules: methods, origins,
          headers, and preflight cache lifetime.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <Tabs value={activeTab} onValueChange={handleTabChange}>
          <TabsList>
            <TabsTrigger value="visual">Visual</TabsTrigger>
            <TabsTrigger value="json">JSON</TabsTrigger>
          </TabsList>
          <TabsContent value="visual" className="space-y-3">
            {corsQ.isPending && (
              <p className="text-sm text-muted-foreground">Loading…</p>
            )}
            {!corsQ.isPending && rules.length === 0 && (
              <p className="text-sm text-muted-foreground">
                No CORS rules configured.
              </p>
            )}
            {rules.map((r, i) => (
              <RuleCard
                key={`${r.id ?? 'rule'}-${i}`}
                rule={r}
                onChange={(updater) => handleUpdateRule(i, updater)}
                onDelete={() => handleDeleteRule(i)}
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
                  uri: 'https://strata.local/schemas/cors.json',
                  modelUri: `inmemory://cors/${bucket.name}.json`,
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
      <CardFooter className="flex items-center justify-between gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="text-destructive"
          disabled={deleting || saving || corsQ.isPending || !corsQ.data}
          onClick={handleDelete}
        >
          {deleting && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
          Delete configuration
        </Button>
        <Button
          type="button"
          size="sm"
          disabled={saving || corsQ.isPending}
          onClick={handleSave}
        >
          {saving && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />}
          Save CORS
        </Button>
      </CardFooter>
    </Card>
  );
}

interface RuleCardProps {
  rule: CORSRule;
  onChange: (updater: (r: CORSRule) => CORSRule) => void;
  onDelete: () => void;
}

function RuleCard({ rule, onChange, onDelete }: RuleCardProps) {
  function toggleMethod(m: HttpMethod) {
    onChange((r) => {
      const has = r.allowed_methods.includes(m);
      const next = has
        ? r.allowed_methods.filter((x) => x !== m)
        : [...r.allowed_methods, m];
      return { ...r, allowed_methods: next };
    });
  }

  return (
    <div className="space-y-3 rounded-md border p-3">
      <div className="flex flex-wrap items-end gap-3">
        <div className="grow space-y-1">
          <Label htmlFor={`cors-id-${rule.id ?? ''}`}>ID (optional)</Label>
          <Input
            id={`cors-id-${rule.id ?? ''}`}
            value={rule.id ?? ''}
            onChange={(e) => onChange((r) => ({ ...r, id: e.target.value }))}
            className="h-9"
            placeholder="allow-app-read"
          />
        </div>
        <div className="ml-auto">
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
          Allowed methods
        </legend>
        <div className="flex flex-wrap gap-2">
          {HTTP_METHODS.map((m) => {
            const active = rule.allowed_methods.includes(m);
            return (
              <button
                type="button"
                key={m}
                onClick={() => toggleMethod(m)}
                className={
                  active
                    ? 'rounded-md border border-primary bg-primary px-2 py-1 text-xs font-medium text-primary-foreground'
                    : 'rounded-md border bg-background px-2 py-1 text-xs text-muted-foreground hover:bg-muted'
                }
              >
                {m}
              </button>
            );
          })}
        </div>
      </fieldset>

      <ChipList
        label="Allowed origins"
        placeholder="https://app.example.com or *"
        values={rule.allowed_origins}
        onChange={(next) => onChange((r) => ({ ...r, allowed_origins: next }))}
      />
      <ChipList
        label="Allowed headers"
        placeholder="Authorization"
        values={rule.allowed_headers ?? []}
        onChange={(next) =>
          onChange((r) => ({
            ...r,
            allowed_headers: next.length === 0 ? undefined : next,
          }))
        }
      />
      <ChipList
        label="Expose headers"
        placeholder="ETag"
        values={rule.expose_headers ?? []}
        onChange={(next) =>
          onChange((r) => ({
            ...r,
            expose_headers: next.length === 0 ? undefined : next,
          }))
        }
      />

      <div className="space-y-1">
        <Label htmlFor={`cors-maxage-${rule.id ?? ''}`}>Max age seconds</Label>
        <Input
          id={`cors-maxage-${rule.id ?? ''}`}
          type="number"
          min={0}
          value={rule.max_age_seconds ?? ''}
          onChange={(e) =>
            onChange((r) => ({
              ...r,
              max_age_seconds: e.target.value ? Number(e.target.value) : undefined,
            }))
          }
          className="h-9 max-w-[180px]"
        />
      </div>
    </div>
  );
}

interface ChipListProps {
  label: string;
  placeholder: string;
  values: string[];
  onChange: (next: string[]) => void;
}

function ChipList({ label, placeholder, values, onChange }: ChipListProps) {
  const [draft, setDraft] = useState('');
  function commit() {
    const trimmed = draft.trim();
    if (!trimmed) return;
    if (values.includes(trimmed)) {
      setDraft('');
      return;
    }
    onChange([...values, trimmed]);
    setDraft('');
  }
  return (
    <fieldset className="space-y-2">
      <legend className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </legend>
      {values.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {values.map((v, i) => (
            <span
              key={`${v}-${i}`}
              className="inline-flex items-center gap-1 rounded-md border bg-muted px-2 py-0.5 text-xs"
            >
              {v}
              <button
                type="button"
                aria-label={`Remove ${v}`}
                onClick={() => onChange(values.filter((_, j) => j !== i))}
                className="text-muted-foreground hover:text-destructive"
              >
                ×
              </button>
            </span>
          ))}
        </div>
      )}
      <div className="flex items-center gap-1">
        <Input
          value={draft}
          placeholder={placeholder}
          className="h-8"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              commit();
            }
          }}
        />
        <Button type="button" variant="outline" size="sm" onClick={commit}>
          Add
        </Button>
      </div>
    </fieldset>
  );
}

function formatJson(cfg: CORSConfig): string {
  return JSON.stringify(cfg, null, 2);
}

function safeParseRules(text: string): CORSRule[] | null {
  try {
    const parsed = JSON.parse(text) as CORSConfig;
    if (parsed && Array.isArray(parsed.rules)) return parsed.rules;
    return null;
  } catch {
    return null;
  }
}
