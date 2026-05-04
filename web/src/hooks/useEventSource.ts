// useEventSource subscribes to a Server-Sent Events endpoint with automatic
// reconnect on disconnect. The browser EventSource API already retries on
// transport-level errors, but the cadence is opaque and proxies sometimes
// terminate the connection cleanly (no error event). This hook layers an
// explicit exponential-backoff reconnect schedule on top so the UI can render
// a `Connection lost — reconnecting in N s` banner with the next-attempt
// countdown.
//
// Designed for US-002 (Live audit-tail UI). The audit/stream endpoint emits
// one `data: <json>` frame per audit row; consumers pass an onMessage that
// JSON-parses the frame and appends to a ring.

import { useEffect, useRef, useState } from 'react';

export type EventSourceState = 'connecting' | 'open' | 'reconnecting' | 'closed';

export interface UseEventSourceOptions {
  // url is the SSE endpoint. When url changes the hook closes the previous
  // EventSource and reopens against the new URL — used by audit-tail to
  // re-subscribe with new filters.
  url: string;
  // enabled gates the subscription; defaults to true. Pause flips this so
  // the connection drops while the user reviews the buffered ring.
  enabled?: boolean;
  // onMessage receives one decoded SSE event. The hook does not parse JSON —
  // callers do, so they can tolerate the `:keep-alive` ping frames and
  // arbitrary payload schemas.
  onMessage: (data: string) => void;
  // withCredentials flips the EventSource credential mode. The admin API
  // sits behind a same-origin session cookie so this is true by default.
  withCredentials?: boolean;
  // initialBackoffMs / maxBackoffMs control the reconnect cadence. Defaults
  // are 1s → 30s, doubling on every consecutive failure.
  initialBackoffMs?: number;
  maxBackoffMs?: number;
}

export interface UseEventSourceResult {
  state: EventSourceState;
  // retryInSeconds is the countdown to the next reconnect attempt while the
  // hook is in `reconnecting` state. 0 means a reconnect is being attempted
  // right now (or the hook is not retrying).
  retryInSeconds: number;
  // disconnects increments every time the connection closes unexpectedly —
  // useful for tests asserting reconnect occurred.
  disconnects: number;
}

const DEFAULT_INITIAL_BACKOFF_MS = 1_000;
const DEFAULT_MAX_BACKOFF_MS = 30_000;

export function useEventSource(opts: UseEventSourceOptions): UseEventSourceResult {
  const {
    url,
    enabled = true,
    onMessage,
    withCredentials = true,
    initialBackoffMs = DEFAULT_INITIAL_BACKOFF_MS,
    maxBackoffMs = DEFAULT_MAX_BACKOFF_MS,
  } = opts;

  const [state, setState] = useState<EventSourceState>('closed');
  const [retryInSeconds, setRetryInSeconds] = useState(0);
  const [disconnects, setDisconnects] = useState(0);

  // Ref-cycle: latest onMessage so the connect effect can read the freshest
  // callback without retriggering reconnect on every re-render.
  const onMessageRef = useRef(onMessage);
  onMessageRef.current = onMessage;

  useEffect(() => {
    if (!enabled || !url) {
      setState('closed');
      setRetryInSeconds(0);
      return;
    }

    let cancelled = false;
    let backoff = initialBackoffMs;
    let es: EventSource | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let countdownTimer: ReturnType<typeof setInterval> | null = null;

    function clearTimers() {
      if (retryTimer != null) {
        clearTimeout(retryTimer);
        retryTimer = null;
      }
      if (countdownTimer != null) {
        clearInterval(countdownTimer);
        countdownTimer = null;
      }
    }

    function connect() {
      if (cancelled) return;
      setState('connecting');
      setRetryInSeconds(0);

      es = new EventSource(url, { withCredentials });

      es.onopen = () => {
        if (cancelled) return;
        backoff = initialBackoffMs;
        setState('open');
        setRetryInSeconds(0);
      };

      es.onmessage = (ev: MessageEvent) => {
        if (cancelled) return;
        // EventSource only delivers `message` for unnamed `data:` lines;
        // `:keep-alive` comments are stripped by the spec parser, so we
        // never see them here.
        if (typeof ev.data === 'string') onMessageRef.current(ev.data);
      };

      es.onerror = () => {
        if (cancelled) return;
        es?.close();
        es = null;
        setDisconnects((n) => n + 1);
        scheduleReconnect();
      };
    }

    function scheduleReconnect() {
      if (cancelled) return;
      const delay = Math.min(backoff, maxBackoffMs);
      backoff = Math.min(backoff * 2, maxBackoffMs);
      setState('reconnecting');
      const startedAt = Date.now();
      setRetryInSeconds(Math.ceil(delay / 1000));
      countdownTimer = setInterval(() => {
        const remaining = delay - (Date.now() - startedAt);
        const secs = remaining > 0 ? Math.ceil(remaining / 1000) : 0;
        setRetryInSeconds(secs);
      }, 250);
      retryTimer = setTimeout(() => {
        if (countdownTimer != null) {
          clearInterval(countdownTimer);
          countdownTimer = null;
        }
        connect();
      }, delay);
    }

    connect();

    return () => {
      cancelled = true;
      clearTimers();
      if (es) {
        es.close();
        es = null;
      }
      setState('closed');
      setRetryInSeconds(0);
    };
  }, [url, enabled, withCredentials, initialBackoffMs, maxBackoffMs]);

  return { state, retryInSeconds, disconnects };
}
