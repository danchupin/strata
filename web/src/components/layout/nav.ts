import {
  Boxes,
  Gauge,
  LayoutDashboard,
  Settings,
  Users,
  type LucideIcon,
} from 'lucide-react';

export interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
}

// Primary nav for the AppShell sidebar. Order is intentional — Overview is
// the home, Settings is a Phase 2 placeholder kept here so the empty entry
// communicates the future shape.
export const primaryNav: NavItem[] = [
  { to: '/', label: 'Overview', icon: LayoutDashboard, end: true },
  { to: '/buckets', label: 'Buckets', icon: Boxes },
  { to: '/consumers', label: 'Consumers', icon: Users },
  { to: '/metrics', label: 'Metrics', icon: Gauge },
  { to: '/settings', label: 'Settings', icon: Settings },
];
