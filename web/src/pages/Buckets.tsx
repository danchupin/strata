import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';

export function BucketsPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Buckets</h1>
        <p className="text-sm text-muted-foreground">
          List, search, and inspect every bucket in the cluster.
        </p>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Buckets list — coming soon</CardTitle>
          <CardDescription>
            Wired in US-010. Bucket detail and the read-only object browser land in US-011.
          </CardDescription>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          Routing is in place (US-005); navigate freely between sidebar items.
        </CardContent>
      </Card>
    </div>
  );
}
