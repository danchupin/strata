import { Navigate, Route, Routes } from 'react-router-dom';

import { Badge } from '@/components/ui/badge';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { ThemeToggle } from '@/components/theme-toggle';
import { UserMenu } from '@/components/user-menu';
import { RequireAuth } from '@/components/require-auth';
import { LoginPage } from '@/pages/Login';

function HomeShell() {
  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="border-b">
        <div className="container flex h-14 items-center justify-between">
          <div className="flex items-center gap-2">
            <span className="text-lg font-semibold tracking-tight">
              Strata Console
            </span>
            <Badge variant="outline">Phase 1</Badge>
          </div>
          <div className="flex items-center gap-2">
            <ThemeToggle />
            <UserMenu />
          </div>
        </div>
      </header>
      <main className="container py-8">
        <Card className="mx-auto max-w-2xl">
          <CardHeader>
            <CardTitle>Strata Console — coming soon</CardTitle>
            <CardDescription>
              Foundation bundle (Phase 1). Login is wired (US-004); cluster
              overview, buckets, and metrics land in following stories.
            </CardDescription>
          </CardHeader>
          <CardContent className="text-sm text-muted-foreground">
            Toggle the theme in the top-right corner. Use the user menu to
            sign out — sessions expire after 24h.
          </CardContent>
        </Card>
      </main>
    </div>
  );
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/*"
        element={
          <RequireAuth>
            <HomeShell />
          </RequireAuth>
        }
      />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
