import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { api } from '@/api/client';

export function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return <label className="field"><span>{label}</span>{children}{hint && <small className="muted">{hint}</small>}</label>;
}
export function useResource<T>(path: string) {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<unknown>();
  const [revision, setRevision] = useState(0);
  const reload = useCallback(() => setRevision(value => value + 1), []);
  useEffect(() => {
    const controller = new AbortController();
    setError(undefined);
    api<T>(path, { signal: controller.signal }).then(setData).catch(err => { if (!controller.signal.aborted) setError(err); });
    return () => controller.abort();
  }, [path, revision]);
  return { data, error, setError, reload, revision };
}
export function date(value?: number) {
  return value ? new Date(value * 1000).toLocaleString('ko-KR') : '—';
}
export interface User { id: string; login_id: string; display_name: string; email?: string; role: string; status: string; auth_provider: string; last_login_at?: number }
export interface MCPKey { id: string; user_id: string; name: string; key_prefix: string; status: string; created_at: number; last_used_at?: number }
export type Settings = Record<string, string>;
