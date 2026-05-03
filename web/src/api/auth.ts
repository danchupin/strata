// Auth client for /admin/v1/auth/*. Browser carries the session cookie
// automatically once login succeeds; we never read the cookie value (it is
// HttpOnly). The whoami endpoint is the source of truth for "am I logged in?".

export interface SessionInfo {
  access_key: string;
  expires_at: number;
}

export interface LoginRequest {
  access_key: string;
  secret_key: string;
}

export interface AdminApiError {
  code: string;
  message: string;
}

export class AuthError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function parseError(resp: Response): Promise<AuthError> {
  let code = 'Error';
  let message = resp.statusText || 'request failed';
  try {
    const body = (await resp.json()) as Partial<AdminApiError>;
    if (body.code) code = body.code;
    if (body.message) message = body.message;
  } catch {
    /* ignore body parse errors */
  }
  return new AuthError(resp.status, code, message);
}

export async function login(req: LoginRequest): Promise<SessionInfo> {
  const resp = await fetch('/admin/v1/auth/login', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  });
  if (!resp.ok) throw await parseError(resp);
  return (await resp.json()) as SessionInfo;
}

export async function logout(): Promise<void> {
  await fetch('/admin/v1/auth/logout', {
    method: 'POST',
    credentials: 'same-origin',
  });
}

export async function whoami(): Promise<SessionInfo | null> {
  const resp = await fetch('/admin/v1/auth/whoami', {
    method: 'GET',
    credentials: 'same-origin',
  });
  if (resp.status === 401) return null;
  if (!resp.ok) throw await parseError(resp);
  return (await resp.json()) as SessionInfo;
}
