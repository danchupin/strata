import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';

export function SettingsPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Settings</h1>
        <p className="text-sm text-muted-foreground">
          Cluster-level configuration and user preferences.
        </p>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Settings — Phase 2</CardTitle>
          <CardDescription>
            Phase 1 is read-only by design; Settings (IAM, write actions, audit viewer) land in prd-web-ui-admin.md.
          </CardDescription>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          Routing placeholder so the sidebar nav has a target during Phase 1.
        </CardContent>
      </Card>
    </div>
  );
}
