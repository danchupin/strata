import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';

import { login as apiLogin, logout as apiLogout, whoami } from '@/api/auth';
import type { LoginRequest, SessionInfo } from '@/api/auth';

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
  const [state, setState] = useState<AuthState>({ status: 'unknown' });

  const refresh = useCallback(async () => {
    try {
      const session = await whoami();
      setState(
        session ? { status: 'authenticated', session } : { status: 'unauthenticated' },
      );
    } catch {
      setState({ status: 'unauthenticated' });
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const login = useCallback(async (req: LoginRequest) => {
    const session = await apiLogin(req);
    setState({ status: 'authenticated', session });
  }, []);

  const logout = useCallback(async () => {
    await apiLogout();
    setState({ status: 'unauthenticated' });
  }, []);

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
