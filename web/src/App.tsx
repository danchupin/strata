import { Suspense, lazy } from 'react';
import { Navigate, Route, Routes } from 'react-router-dom';

import { AppShell } from '@/components/layout/AppShell';
import { RequireAuth } from '@/components/require-auth';
import { Skeleton } from '@/components/ui/skeleton';
import { LoginPage } from '@/pages/Login';
import { OverviewPage } from '@/pages/Overview';
import { BucketsPage } from '@/pages/Buckets';
import { BucketDetailPage } from '@/pages/BucketDetail';
import { ConsumersPage } from '@/pages/Consumers';
import { IAMPage } from '@/pages/IAM';
import { IAMUserDetailPage } from '@/pages/IAMUserDetail';
import { MultipartPage } from '@/pages/Multipart';
import { SettingsPage } from '@/pages/Settings';

// Metrics page lazy-loads recharts (~110 KiB gzipped) only when the operator
// navigates to /metrics — keeps the home-page initial bundle small.
const MetricsPage = lazy(() =>
  import('@/pages/Metrics').then((m) => ({ default: m.MetricsPage })),
);

function PageFallback() {
  return (
    <div className="space-y-4">
      <Skeleton className="h-8 w-48" />
      <Skeleton className="h-56 w-full" />
    </div>
  );
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        element={
          <RequireAuth>
            <AppShell />
          </RequireAuth>
        }
      >
        <Route path="/" element={<OverviewPage />} />
        <Route path="/buckets" element={<BucketsPage />} />
        <Route path="/buckets/:name" element={<BucketDetailPage />} />
        <Route path="/consumers" element={<ConsumersPage />} />
        <Route path="/iam" element={<IAMPage />} />
        <Route path="/iam/users/:userName" element={<IAMUserDetailPage />} />
        <Route path="/multipart" element={<MultipartPage />} />
        <Route
          path="/metrics"
          element={
            <Suspense fallback={<PageFallback />}>
              <MetricsPage />
            </Suspense>
          }
        />
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
