import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';

export function ConsumersPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Consumers</h1>
        <p className="text-sm text-muted-foreground">
          Top access keys by request count and bytes over the last 24h.
        </p>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Consumers — coming soon</CardTitle>
          <CardDescription>
            Top-consumers widget on the Overview page lands in US-007; a dedicated page follows in Phase 2.
          </CardDescription>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          Routing placeholder so the sidebar nav has a target during Phase 1.
        </CardContent>
      </Card>
    </div>
  );
}
