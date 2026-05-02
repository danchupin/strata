import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';

export function MetricsPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Metrics</h1>
        <p className="text-sm text-muted-foreground">
          Request rate, latency, error rate, and bytes over a selectable window.
        </p>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Metrics dashboard — coming soon</CardTitle>
          <CardDescription>
            Charts (recharts 2.x), time-range selector, and per-range polling cadence land in US-009.
          </CardDescription>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          Routing placeholder so the sidebar nav has a target during Phase 1.
        </CardContent>
      </Card>
    </div>
  );
}
