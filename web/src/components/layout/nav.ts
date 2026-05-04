import {
  Activity,
  Boxes,
  ClipboardList,
  Gauge,
  KeyRound,
  LayoutDashboard,
  Layers,
  Network,
  Settings,
  Timer,
  Users,
  type LucideIcon,
} from 'lucide-react';

export interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
}

export interface NavSection {
  // label is optional; the first section renders without a header so the
  // primary entries (Overview, Buckets, …) keep their existing flat look.
  label?: string;
  items: NavItem[];
}

// primaryNav drives the AppShell sidebar. The first section is the Phase 1+2
// surface; the Diagnostics section was added in Phase 3 (US-002 onward) so
// debug-only pages stay grouped without polluting the main nav.
export const primaryNav: NavSection[] = [
  {
    items: [
      { to: '/', label: 'Overview', icon: LayoutDashboard, end: true },
      { to: '/buckets', label: 'Buckets', icon: Boxes },
      { to: '/consumers', label: 'Consumers', icon: Users },
      { to: '/iam', label: 'IAM', icon: KeyRound },
      { to: '/multipart', label: 'Multipart', icon: Layers },
      { to: '/audit', label: 'Audit log', icon: ClipboardList },
      { to: '/metrics', label: 'Metrics', icon: Gauge },
      { to: '/settings', label: 'Settings', icon: Settings },
    ],
  },
  {
    label: 'Diagnostics',
    items: [
      { to: '/diagnostics/audit-tail', label: 'Audit tail', icon: Activity },
      { to: '/diagnostics/slow-queries', label: 'Slow queries', icon: Timer },
      { to: '/diagnostics/trace', label: 'Trace browser', icon: Network },
    ],
  },
];
