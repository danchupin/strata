import { NavLink } from 'react-router-dom';

import { cn } from '@/lib/utils';
import { primaryNav } from '@/components/layout/nav';

interface SidebarNavProps {
  collapsed: boolean;
  onNavigate?: () => void;
}

// SidebarNav renders the primary nav list. Used both in the desktop sidebar
// (collapsed=true → icon-only) and inside the mobile Sheet (collapsed=false,
// onNavigate closes the sheet on selection).
export function SidebarNav({ collapsed, onNavigate }: SidebarNavProps) {
  return (
    <nav className="flex flex-col gap-1 px-2 py-3">
      {primaryNav.map((item) => {
        const Icon = item.icon;
        return (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            onClick={onNavigate}
            title={collapsed ? item.label : undefined}
            className={({ isActive }) =>
              cn(
                'group flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors',
                'text-muted-foreground hover:bg-accent hover:text-accent-foreground',
                'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
                isActive && 'bg-accent text-accent-foreground',
                collapsed && 'justify-center px-2',
              )
            }
          >
            <Icon className="h-4 w-4 shrink-0" aria-hidden />
            <span className={cn('truncate', collapsed && 'sr-only')}>
              {item.label}
            </span>
          </NavLink>
        );
      })}
    </nav>
  );
}
