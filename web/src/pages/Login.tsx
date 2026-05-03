import { useState, type FormEvent } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';

import { AuthError } from '@/api/client';
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
import { useAuth } from '@/components/auth-provider';

interface LocationState {
  from?: { pathname: string };
}

export function LoginPage() {
  const [accessKey, setAccessKey] = useState('');
  const [secretKey, setSecretKey] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const { login } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  const redirectTo =
    (location.state as LocationState | null)?.from?.pathname ?? '/';

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await login({ access_key: accessKey, secret_key: secretKey });
      navigate(redirectTo, { replace: true });
    } catch (err) {
      if (err instanceof AuthError) {
        setError(err.message);
      } else {
        setError('Login failed. Check your credentials and try again.');
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>Strata Console</CardTitle>
          <CardDescription>
            Sign in with your IAM access key and secret.
          </CardDescription>
        </CardHeader>
        <form onSubmit={handleSubmit}>
          <CardContent className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="access_key">Access Key</Label>
              <Input
                id="access_key"
                name="access_key"
                autoComplete="username"
                autoFocus
                required
                value={accessKey}
                onChange={(e) => setAccessKey(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="secret_key">Secret Key</Label>
              <Input
                id="secret_key"
                name="secret_key"
                type="password"
                autoComplete="current-password"
                required
                value={secretKey}
                onChange={(e) => setSecretKey(e.target.value)}
              />
            </div>
            {error && (
              <p
                role="alert"
                className="text-sm text-destructive"
                data-testid="login-error"
              >
                {error}
              </p>
            )}
          </CardContent>
          <CardFooter>
            <Button type="submit" className="w-full" disabled={submitting}>
              {submitting ? 'Signing in…' : 'Sign in'}
            </Button>
          </CardFooter>
        </form>
      </Card>
    </div>
  );
}
