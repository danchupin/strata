// Web Worker that owns a single multipart-or-single-PUT upload. Posts
// progress updates back to the parent UI thread so the dialog can render
// per-file progress bars without blocking on the slicing / hashing /
// network I/O.
//
// Wire shape:
//
//  - parent → worker (start):
//      { kind: 'start',
//        bucket: string,
//        file: File,
//        path: string,            // operator-chosen object key
//        partSize: number,        // bytes per part (init response)
//        uploadId: string,        // multipart upload id (init response)
//        singleUrl?: string }     // when set the worker performs ONE PUT
//
//  - worker → parent:
//      { kind: 'progress', uploaded: number, total: number }
//      { kind: 'partDone', partNumber: number, etag: string }
//      { kind: 'done', etag: string }     // final etag (composite for MPU)
//      { kind: 'error', message: string }
//
//  - parent → worker (abort):
//      { kind: 'abort' }
//
// The worker requests presigned URLs through the parent thread (not directly
// from /admin/v1/* — workers cannot share session cookies in some browsers),
// so per-part presign happens on the parent and the URL is shipped down for
// the worker's PUT. The parent calls presignUploadPart(), then posts:
//
//      { kind: 'partUrl', partNumber: number, url: string }
//
// in response to a worker → parent { kind: 'needPartUrl', partNumber }.

export type WorkerToParent =
  | { kind: 'progress'; uploaded: number; total: number }
  | { kind: 'partDone'; partNumber: number; etag: string }
  | { kind: 'needPartUrl'; partNumber: number }
  | { kind: 'done'; etag: string }
  | { kind: 'error'; message: string };

export type ParentToWorker =
  | {
      kind: 'startMultipart';
      file: Blob;
      partSize: number;
      uploadId: string;
    }
  | { kind: 'startSingle'; file: Blob; url: string; storageClass?: string }
  | { kind: 'partUrl'; partNumber: number; url: string }
  | { kind: 'abort' };

interface PendingPart {
  partNumber: number;
  resolve: (url: string) => void;
}

let aborted = false;
const pendingPartURLs: PendingPart[] = [];

self.onmessage = (event: MessageEvent<ParentToWorker>) => {
  const msg = event.data;
  switch (msg.kind) {
    case 'startSingle':
      void runSinglePut(msg.file, msg.url, msg.storageClass);
      break;
    case 'startMultipart':
      void runMultipart(msg.file, msg.partSize, msg.uploadId);
      break;
    case 'partUrl': {
      const idx = pendingPartURLs.findIndex((p) => p.partNumber === msg.partNumber);
      if (idx >= 0) {
        pendingPartURLs[idx].resolve(msg.url);
        pendingPartURLs.splice(idx, 1);
      }
      break;
    }
    case 'abort':
      aborted = true;
      break;
  }
};

function post(msg: WorkerToParent): void {
  (self as unknown as Worker).postMessage(msg);
}

async function runSinglePut(file: Blob, url: string, storageClass?: string): Promise<void> {
  try {
    const headers: Record<string, string> = {};
    if (storageClass) headers['x-amz-storage-class'] = storageClass;
    const etag = await putWithProgress(url, file, file.size, 0, headers);
    post({ kind: 'progress', uploaded: file.size, total: file.size });
    post({ kind: 'done', etag });
  } catch (err) {
    if (aborted) return;
    post({ kind: 'error', message: String((err as Error).message ?? err) });
  }
}

async function runMultipart(file: Blob, partSize: number, _uploadId: string): Promise<void> {
  try {
    const total = file.size;
    const parts: { partNumber: number; etag: string }[] = [];
    let uploaded = 0;
    const partCount = Math.max(1, Math.ceil(total / partSize));
    for (let i = 0; i < partCount; i++) {
      if (aborted) return;
      const partNumber = i + 1;
      const offset = i * partSize;
      const end = Math.min(offset + partSize, total);
      const chunk = file.slice(offset, end);
      const url = await requestPartURL(partNumber);
      const etag = await putWithProgress(url, chunk, total, uploaded);
      uploaded += end - offset;
      post({ kind: 'progress', uploaded, total });
      post({ kind: 'partDone', partNumber, etag });
      parts.push({ partNumber, etag });
    }
    // The parent owns the Complete call; we just signal "all parts up".
    // The final etag here is "<count>-multipart" to keep the message shape
    // uniform with the single-PUT path; parent ignores the value.
    post({ kind: 'done', etag: `${parts.length}-multipart` });
  } catch (err) {
    if (aborted) return;
    post({ kind: 'error', message: String((err as Error).message ?? err) });
  }
}

function requestPartURL(partNumber: number): Promise<string> {
  return new Promise((resolve) => {
    pendingPartURLs.push({ partNumber, resolve });
    post({ kind: 'needPartUrl', partNumber });
  });
}

// putWithProgress streams `body` to `url` via XHR PUT (fetch lacks upload
// progress events) and resolves with the server-issued ETag. Posts
// incremental { kind: 'progress' } messages with cumulative bytes.
function putWithProgress(
  url: string,
  body: Blob,
  total: number,
  base: number,
  headers?: Record<string, string>,
): Promise<string> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', url, true);
    if (headers) {
      for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
    }
    xhr.upload.onprogress = (ev) => {
      if (!ev.lengthComputable) return;
      post({ kind: 'progress', uploaded: base + ev.loaded, total });
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        const etag = (xhr.getResponseHeader('ETag') ?? '').replace(/^"|"$/g, '');
        resolve(etag);
      } else {
        reject(new Error(`PUT ${xhr.status}: ${xhr.responseText || xhr.statusText}`));
      }
    };
    xhr.onerror = () => reject(new Error('network error'));
    xhr.onabort = () => reject(new Error('aborted'));
    xhr.send(body);
  });
}

export {};
