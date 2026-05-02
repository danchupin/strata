import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';

export function OverviewPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">
          Cluster Overview
        </h1>
        <p className="text-sm text-muted-foreground">
          Cluster health, nodes, and top-level activity at a glance.
        </p>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Cluster Overview — coming soon</CardTitle>
          <CardDescription>
            Hero card, nodes table, and top-buckets / top-consumers widgets land in US-006 and US-007.
          </CardDescription>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          The shell, sidebar, top bar, and routing are wired (US-005). Pick a
          nav item to see the corresponding placeholder page; future stories
          fill the content in.
        </CardContent>
      </Card>
    </div>
  );
}
