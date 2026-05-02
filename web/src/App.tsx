import { Navigate, Route, Routes } from 'react-router-dom';

import { AppShell } from '@/components/layout/AppShell';
import { RequireAuth } from '@/components/require-auth';
import { LoginPage } from '@/pages/Login';
import { OverviewPage } from '@/pages/Overview';
import { BucketsPage } from '@/pages/Buckets';
import { ConsumersPage } from '@/pages/Consumers';
import { MetricsPage } from '@/pages/Metrics';
import { SettingsPage } from '@/pages/Settings';

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
        <Route path="/consumers" element={<ConsumersPage />} />
        <Route path="/metrics" element={<MetricsPage />} />
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
