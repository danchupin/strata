import { Badge } from '@/components/ui/badge';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { ThemeToggle } from '@/components/theme-toggle';

export function App() {
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
          <ThemeToggle />
        </div>
      </header>
      <main className="container py-8">
        <Card className="mx-auto max-w-2xl">
          <CardHeader>
            <CardTitle>Strata Console — coming soon</CardTitle>
            <CardDescription>
              Foundation bundle (Phase 1, US-002). Tailwind + shadcn/ui design
              system + dark mode. Next stories add login, cluster overview,
              buckets, metrics.
            </CardDescription>
          </CardHeader>
          <CardContent className="text-sm text-muted-foreground">
            Toggle the theme in the top-right corner. Light, dark, and system
            modes are persisted to <code>localStorage['strata.theme']</code>.
          </CardContent>
        </Card>
      </main>
    </div>
  );
}
