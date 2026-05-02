import { useSyncExternalStore } from 'react';

export interface ToastRecord {
  id: string;
  title?: string;
  description?: string;
  variant?: 'default' | 'destructive';
  action?: { label: string; onClick: () => void };
  durationMs?: number;
}

type Listener = () => void;

const listeners = new Set<Listener>();
let toasts: ToastRecord[] = [];

function emit() {
  for (const l of listeners) l();
}

function nextId(): string {
  return `t_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
}

export function showToast(t: Omit<ToastRecord, 'id'>): string {
  const id = nextId();
  toasts = [...toasts, { id, ...t }];
  emit();
  return id;
}

export function dismissToast(id: string) {
  toasts = toasts.filter((t) => t.id !== id);
  emit();
}

function subscribe(l: Listener): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

function snapshot(): ToastRecord[] {
  return toasts;
}

export function useToasts(): ToastRecord[] {
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}
