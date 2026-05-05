import { useEffect, useMemo, useRef, useState } from 'react';
import { AlertCircle, Loader2, UploadCloud, X } from 'lucide-react';

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
  abortUpload,
  completeUpload,
  initiateUpload,
  presignSinglePut,
  presignUploadPart,
  type AdminApiError,
  type UploadCompletePart,
} from '@/api/client';
import { queryClient, queryKeys } from '@/lib/query';
import { showToast } from '@/lib/toast-store';
import UploadWorker from '@/workers/upload?worker';
import type { ParentToWorker, WorkerToParent } from '@/workers/upload';

// Files <=5 MiB go through the single-PUT path (no multipart bookkeeping);
// larger files use the multipart upload + per-part presign flow.
const SINGLE_PUT_MAX_BYTES = 5 * 1024 * 1024;

interface Props {
  open: boolean;
  bucket: string;
  prefix: string;
  onOpenChange: (open: boolean) => void;
}

interface PerFileState {
  file: File;
  key: string;
  uploaded: number;
  total: number;
  status: 'pending' | 'running' | 'done' | 'error' | 'aborted';
  errorMessage?: string;
  uploadId?: string;
  worker?: Worker;
  parts: UploadCompletePart[];
}

const STORAGE_CLASSES = ['STANDARD', 'STANDARD_IA', 'GLACIER', 'DEEP_ARCHIVE'];
const ENCRYPTION_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'None' },
  { value: 'AES256', label: 'AES256 (SSE-S3)' },
  { value: 'aws:kms', label: 'aws:kms (SSE-KMS)' },
];

export function UploadDialog({ open, bucket, prefix, onOpenChange }: Props) {
  const [files, setFiles] = useState<PerFileState[]>([]);
  const [storageClass, setStorageClass] = useState('STANDARD');
  const [encryption, setEncryption] = useState('');
  const [kmsKeyId, setKmsKeyId] = useState('');
  const [tagsRaw, setTagsRaw] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [globalError, setGlobalError] = useState<string | null>(null);
  const filesRef = useRef<PerFileState[]>([]);
  filesRef.current = files;

  useEffect(() => {
    if (!open) {
      // tear down any running worker on close
      filesRef.current.forEach((f) => f.worker?.terminate());
      setFiles([]);
      setSubmitting(false);
      setGlobalError(null);
      setStorageClass('STANDARD');
      setEncryption('');
      setKmsKeyId('');
      setTagsRaw('');
    }
  }, [open]);

  const tagPairs = useMemo(() => parseTagsRaw(tagsRaw), [tagsRaw]);
  const tagsValid = tagPairs.error === null;
  const overallProgress = useMemo(() => {
    if (files.length === 0) return 0;
    const total = files.reduce((s, f) => s + f.total, 0);
    const done = files.reduce((s, f) => s + f.uploaded, 0);
    return total === 0 ? 0 : Math.round((done * 100) / total);
  }, [files]);
  const allDone =
    files.length > 0 && files.every((f) => f.status === 'done' || f.status === 'aborted');
  const anyRunning = files.some((f) => f.status === 'running');

  function onFilesPicked(picked: FileList | null) {
    if (!picked) return;
    const next: PerFileState[] = Array.from(picked).map((file) => ({
      file,
      key: prefix ? `${prefix.replace(/\/$/, '')}/${file.name}` : file.name,
      uploaded: 0,
      total: file.size,
      status: 'pending',
      parts: [],
    }));
    setFiles((prev) => [...prev, ...next]);
  }

  function updateFile(index: number, patch: Partial<PerFileState>) {
    setFiles((prev) => prev.map((f, i) => (i === index ? { ...f, ...patch } : f)));
  }

  async function handleStart() {
    if (submitting || files.length === 0) return;
    if (!tagsValid) {
      setGlobalError(tagPairs.error ?? 'invalid tags');
      return;
    }
    setGlobalError(null);
    setSubmitting(true);
    try {
      for (let i = 0; i < files.length; i++) {
        const f = filesRef.current[i];
        if (f.status === 'done' || f.status === 'running') continue;
        if (f.file.size <= SINGLE_PUT_MAX_BYTES) {
          await runSinglePut(i);
        } else {
          await runMultipart(i);
        }
      }
      // After every file is done, refresh the objects list so the operator
      // sees their freshly-uploaded files immediately.
      void queryClient.invalidateQueries({ queryKey: queryKeys.buckets.one(bucket) });
      void queryClient.invalidateQueries({ queryKey: ['objects', bucket] });
      showToast({
        title: 'Upload complete',
        description: `${files.length} file${files.length === 1 ? '' : 's'} uploaded to ${bucket}`,
      });
    } catch (err) {
      const e = err as Error;
      setGlobalError(e.message ?? String(err));
    } finally {
      setSubmitting(false);
    }
  }

  async function runSinglePut(index: number): Promise<void> {
    const f = filesRef.current[index];
    updateFile(index, { status: 'running' });
    let presign;
    try {
      presign = await presignSinglePut(bucket, f.key, storageClass);
    } catch (err) {
      const e = err as AdminApiError;
      updateFile(index, { status: 'error', errorMessage: e.message ?? String(err) });
      throw err;
    }
    const worker = new UploadWorker();
    updateFile(index, { worker });
    return new Promise<void>((resolve, reject) => {
      worker.onmessage = (event: MessageEvent<WorkerToParent>) => {
        const msg = event.data;
        if (msg.kind === 'progress') {
          updateFile(index, { uploaded: msg.uploaded, total: msg.total });
        } else if (msg.kind === 'done') {
          worker.terminate();
          updateFile(index, { status: 'done', uploaded: f.file.size });
          resolve();
        } else if (msg.kind === 'error') {
          worker.terminate();
          updateFile(index, { status: 'error', errorMessage: msg.message });
          reject(new Error(msg.message));
        }
      };
      const start: ParentToWorker = {
        kind: 'startSingle',
        file: f.file,
        url: presign.url,
        storageClass: presign.storage_class || undefined,
      };
      worker.postMessage(start);
    });
  }

  async function runMultipart(index: number): Promise<void> {
    const f = filesRef.current[index];
    updateFile(index, { status: 'running' });
    let init;
    try {
      init = await initiateUpload(bucket, {
        key: f.key,
        storage_class: storageClass,
        tags: tagPairs.value,
      });
    } catch (err) {
      const e = err as AdminApiError;
      updateFile(index, { status: 'error', errorMessage: e.message ?? String(err) });
      throw err;
    }
    updateFile(index, { uploadId: init.upload_id });
    const worker = new UploadWorker();
    updateFile(index, { worker });
    const collectedParts: UploadCompletePart[] = [];
    return new Promise<void>((resolve, reject) => {
      worker.onmessage = async (event: MessageEvent<WorkerToParent>) => {
        const msg = event.data;
        if (msg.kind === 'progress') {
          updateFile(index, { uploaded: msg.uploaded, total: msg.total });
        } else if (msg.kind === 'partDone') {
          collectedParts.push({ part_number: msg.partNumber, etag: msg.etag });
        } else if (msg.kind === 'needPartUrl') {
          try {
            const presign = await presignUploadPart(bucket, init.upload_id, msg.partNumber);
            const reply: ParentToWorker = {
              kind: 'partUrl',
              partNumber: msg.partNumber,
              url: presign.url,
            };
            worker.postMessage(reply);
          } catch (err) {
            const e = err as AdminApiError;
            worker.terminate();
            updateFile(index, { status: 'error', errorMessage: e.message ?? String(err) });
            reject(err);
          }
        } else if (msg.kind === 'done') {
          worker.terminate();
          collectedParts.sort((a, b) => a.part_number - b.part_number);
          try {
            await completeUpload(bucket, init.upload_id, collectedParts);
            updateFile(index, { status: 'done', uploaded: f.file.size, parts: collectedParts });
            resolve();
          } catch (err) {
            const e = err as AdminApiError;
            updateFile(index, { status: 'error', errorMessage: e.message ?? String(err) });
            reject(err);
          }
        } else if (msg.kind === 'error') {
          worker.terminate();
          updateFile(index, { status: 'error', errorMessage: msg.message });
          // best-effort abort so dangling parts go to GC
          if (init?.upload_id) {
            void abortUpload(bucket, init.upload_id).catch(() => undefined);
          }
          reject(new Error(msg.message));
        }
      };
      const start: ParentToWorker = {
        kind: 'startMultipart',
        file: f.file,
        partSize: init.part_size,
        uploadId: init.upload_id,
      };
      worker.postMessage(start);
    });
  }

  function handleAbortFile(index: number) {
    const f = filesRef.current[index];
    if (!f.worker || f.status !== 'running') return;
    const abort: ParentToWorker = { kind: 'abort' };
    f.worker.postMessage(abort);
    f.worker.terminate();
    updateFile(index, { status: 'aborted', errorMessage: 'aborted' });
    if (f.uploadId) {
      void abortUpload(bucket, f.uploadId).catch(() => undefined);
    }
  }

  function handleRemoveFile(index: number) {
    const f = filesRef.current[index];
    f.worker?.terminate();
    setFiles((prev) => prev.filter((_, i) => i !== index));
  }

  return (
    <Dialog open={open} onOpenChange={(v) => !anyRunning && onOpenChange(v)}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Upload objects</DialogTitle>
          <DialogDescription>
            Files {`>`} 5 MiB stream as multipart uploads via per-part presigned URLs minted by the
            admin gateway. Operator credentials never reach the browser.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="storage-class">Storage class</Label>
              <select
                id="storage-class"
                className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm shadow-xs"
                value={storageClass}
                onChange={(e) => setStorageClass(e.target.value)}
              >
                {STORAGE_CLASSES.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="encryption">Encryption</Label>
              <select
                id="encryption"
                className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm shadow-xs"
                value={encryption}
                onChange={(e) => setEncryption(e.target.value)}
              >
                {ENCRYPTION_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </div>
          </div>
          {encryption === 'aws:kms' && (
            <div className="space-y-1.5">
              <Label htmlFor="kms-key">KMS Key ID</Label>
              <Input
                id="kms-key"
                placeholder="arn:aws:kms:…/key-id"
                value={kmsKeyId}
                onChange={(e) => setKmsKeyId(e.target.value)}
              />
            </div>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="tags">
              Tags (one per line, <code>key=value</code>)
            </Label>
            <textarea
              id="tags"
              rows={2}
              className="w-full rounded-md border border-input bg-background px-3 py-1 text-sm font-mono shadow-xs"
              placeholder="env=prod&#10;owner=alice"
              value={tagsRaw}
              onChange={(e) => setTagsRaw(e.target.value)}
            />
            {tagPairs.error && (
              <div className="text-xs text-destructive">{tagPairs.error}</div>
            )}
          </div>

          <div className="space-y-2 rounded-md border p-3">
            <div className="flex items-center justify-between">
              <Label htmlFor="file-picker">Files (multi-select)</Label>
              <Input
                id="file-picker"
                type="file"
                multiple
                onChange={(e) => onFilesPicked(e.target.files)}
                className="h-9 max-w-xs"
              />
            </div>
            {files.length === 0 ? (
              <div className="py-6 text-center text-xs text-muted-foreground">
                No files selected. Click above to add files.
              </div>
            ) : (
              <ul className="space-y-2">
                {files.map((f, i) => (
                  <li key={i} className="rounded border p-2 text-sm">
                    <div className="flex items-center justify-between gap-2">
                      <div className="flex flex-col">
                        <span className="font-mono text-xs">{f.key}</span>
                        <span className="text-xs text-muted-foreground">
                          {formatBytes(f.file.size)}
                          {f.errorMessage && (
                            <span className="ml-2 text-destructive">
                              <AlertCircle className="inline h-3 w-3" /> {f.errorMessage}
                            </span>
                          )}
                        </span>
                      </div>
                      <div className="flex items-center gap-1">
                        {f.status === 'running' && (
                          <Button
                            type="button"
                            size="sm"
                            variant="outline"
                            onClick={() => handleAbortFile(i)}
                          >
                            Abort
                          </Button>
                        )}
                        {f.status === 'pending' && (
                          <Button
                            type="button"
                            size="icon"
                            variant="ghost"
                            onClick={() => handleRemoveFile(i)}
                            aria-label={`Remove ${f.key}`}
                          >
                            <X className="h-3.5 w-3.5" />
                          </Button>
                        )}
                        <span className="text-xs">
                          {f.status === 'done'
                            ? '✓'
                            : f.status === 'error'
                              ? '✗'
                              : f.status === 'aborted'
                                ? '·'
                                : `${pct(f.uploaded, f.total)}%`}
                        </span>
                      </div>
                    </div>
                    <div className="mt-1.5 h-1.5 w-full overflow-hidden rounded bg-muted">
                      <div
                        className="h-full bg-primary transition-all"
                        style={{ width: `${pct(f.uploaded, f.total)}%` }}
                      />
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </div>

          {files.length > 1 && (
            <div className="text-xs text-muted-foreground">
              Overall progress:{' '}
              <span className="font-medium text-foreground">{overallProgress}%</span>
            </div>
          )}

          {globalError && (
            <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
              <span>{globalError}</span>
            </div>
          )}
        </div>

        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={anyRunning}
          >
            {allDone ? 'Close' : 'Cancel'}
          </Button>
          <Button onClick={handleStart} disabled={submitting || files.length === 0 || allDone}>
            {submitting && <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" />}
            <UploadCloud className="mr-2 h-3.5 w-3.5" />
            Upload {files.length} file{files.length === 1 ? '' : 's'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function pct(a: number, b: number): number {
  if (b <= 0) return 0;
  return Math.min(100, Math.round((a * 100) / b));
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  let v = bytes;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`;
}

function parseTagsRaw(
  raw: string,
): { value: Record<string, string>; error: string | null } {
  const out: Record<string, string> = {};
  const lines = raw.split('\n').map((l) => l.trim()).filter(Boolean);
  for (const line of lines) {
    const eq = line.indexOf('=');
    if (eq <= 0) {
      return { value: {}, error: `bad tag line: ${line} (expected key=value)` };
    }
    const k = line.slice(0, eq).trim();
    const v = line.slice(eq + 1).trim();
    if (!k) return { value: {}, error: 'tag key cannot be empty' };
    out[k] = v;
  }
  return { value: out, error: null };
}
