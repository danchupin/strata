import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  type ReactNode,
} from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';

import { login as apiLogin, logout as apiLogout, whoami } from '@/api/client';
import type { LoginRequest, SessionInfo } from '@/api/client';
import { queryKeys } from '@/lib/query';

type AuthState =
  | { status: 'unknown' }
  | { status: 'unauthenticated' }
  | { status: 'authenticated'; session: SessionInfo };

interface AuthContextValue {
  state: AuthState;
  login: (req: LoginRequest) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | undefined>(undefined);

export function AuthProvider({ children }: { children: ReactNode }) {
  const qc = useQueryClient();

  // whoami() returns null on 401; useQuery surfaces null as data so the toast
  // pipeline never fires for unauth (only real errors). meta.silent guards
  // against a 5xx whoami toast on the login page.
  const probe = useQuery({
    queryKey: queryKeys.auth.whoami,
    queryFn: whoami,
    staleTime: 60_000,
    refetchInterval: false,
    refetchOnWindowFocus: true,
    retry: 0,
    meta: { label: 'session', silent: true },
  });

  const state: AuthState = (() => {
    if (probe.isPending) return { status: 'unknown' };
    if (probe.data) return { status: 'authenticated', session: probe.data };
    return { status: 'unauthenticated' };
  })();

  const refresh = useCallback(async () => {
    await qc.invalidateQueries({ queryKey: queryKeys.auth.whoami });
  }, [qc]);

  const login = useCallback(
    async (req: LoginRequest) => {
      const session = await apiLogin(req);
      qc.setQueryData(queryKeys.auth.whoami, session);
    },
    [qc],
  );

  const logout = useCallback(async () => {
    await apiLogout();
    qc.setQueryData(queryKeys.auth.whoami, null);
    // Drop any data that was fetched under the old session so a different user
    // signing in next does not see stale rows for a moment.
    qc.removeQueries();
    qc.setQueryData(queryKeys.auth.whoami, null);
  }, [qc]);

  const value = useMemo<AuthContextValue>(
    () => ({ state, login, logout, refresh }),
    [state, login, logout, refresh],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used inside <AuthProvider>');
  return ctx;
}
