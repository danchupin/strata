import { useEffect, useState } from 'react';
import { Outlet } from 'react-router-dom';
import {
  ChevronLeft,
  ChevronRight,
  Menu,
  Search,
} from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from '@/components/ui/sheet';
import { ThemeToggle } from '@/components/theme-toggle';
import { UserMenu } from '@/components/user-menu';
import { SidebarNav } from '@/components/layout/SidebarNav';
import { cn } from '@/lib/utils';
import { fetchClusterStatus, type ClusterStatus } from '@/api/cluster';

const COLLAPSED_KEY = 'strata.sidebar.collapsed';
const NARROW_QUERY = '(max-width: 1023px)';
const MOBILE_QUERY = '(max-width: 639px)';

function readCollapsed(): boolean {
  if (typeof window === 'undefined') return false;
  return window.localStorage.getItem(COLLAPSED_KEY) === '1';
}

// useMediaQuery returns true when the document matches the supplied media
// query. Re-runs on viewport changes so the layout reacts to resize without
// a reload.
function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return false;
    return window.matchMedia(query).matches;
  });
  useEffect(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return;
    const mql = window.matchMedia(query);
    const handler = (e: MediaQueryListEvent) => setMatches(e.matches);
    setMatches(mql.matches);
    mql.addEventListener('change', handler);
    return () => mql.removeEventListener('change', handler);
  }, [query]);
  return matches;
}

// useClusterStatus polls /admin/v1/cluster/status until US-008 wires
// TanStack Query. Refresh is one-shot per mount + the auth provider
// retriggers via component mount. Good enough for the top-bar cluster name.
function useClusterStatus(): ClusterStatus | null {
  const [status, setStatus] = useState<ClusterStatus | null>(null);
  useEffect(() => {
    let cancelled = false;
    fetchClusterStatus()
      .then((s) => {
        if (!cancelled) setStatus(s);
      })
      .catch(() => {
        if (!cancelled) setStatus(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);
  return status;
}

export function AppShell() {
  const isMobile = useMediaQuery(MOBILE_QUERY);
  const isNarrow = useMediaQuery(NARROW_QUERY);
  const [persistedCollapsed, setPersistedCollapsed] = useState<boolean>(readCollapsed);
  const [mobileOpen, setMobileOpen] = useState(false);
  const status = useClusterStatus();

  // On viewports <1024 px the sidebar forces icon-only; user toggle still
  // controls ≥1024 px.
  const collapsed = isNarrow || persistedCollapsed;

  useEffect(() => {
    if (typeof window === 'undefined') return;
    window.localStorage.setItem(COLLAPSED_KEY, persistedCollapsed ? '1' : '0');
  }, [persistedCollapsed]);

  const clusterName = status?.cluster_name ?? 'Strata';

  return (
    <div className="flex min-h-screen bg-background text-foreground">
      {!isMobile && (
        <aside
          className={cn(
            'sticky top-0 flex h-screen flex-col border-r bg-background transition-[width] duration-200',
            collapsed ? 'w-14' : 'w-60',
          )}
          aria-label="Primary navigation"
        >
          <div
            className={cn(
              'flex h-14 items-center border-b px-3',
              collapsed ? 'justify-center' : 'justify-between',
            )}
          >
            {!collapsed && (
              <span className="text-sm font-semibold tracking-tight">
                Strata
              </span>
            )}
            {!isNarrow && (
              <Button
                variant="ghost"
                size="icon"
                className="h-7 w-7"
                aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
                onClick={() => setPersistedCollapsed((v) => !v)}
              >
                {collapsed ? (
                  <ChevronRight className="h-4 w-4" />
                ) : (
                  <ChevronLeft className="h-4 w-4" />
                )}
              </Button>
            )}
          </div>
          <div className="flex-1 overflow-y-auto">
            <SidebarNav collapsed={collapsed} />
          </div>
        </aside>
      )}

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-30 flex h-14 items-center gap-3 border-b bg-background/95 px-4 backdrop-blur supports-[backdrop-filter]:bg-background/75">
          {isMobile && (
            <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
              <SheetTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-9 w-9"
                  aria-label="Open navigation"
                >
                  <Menu className="h-5 w-5" />
                </Button>
              </SheetTrigger>
              <SheetContent side="left" className="w-64 p-0">
                <SheetTitle className="sr-only">Navigation</SheetTitle>
                <div className="flex h-14 items-center border-b px-4 text-sm font-semibold">
                  Strata
                </div>
                <SidebarNav collapsed={false} onNavigate={() => setMobileOpen(false)} />
              </SheetContent>
            </Sheet>
          )}

          <div className="flex min-w-0 items-center gap-2">
            <span className="truncate text-sm font-semibold tracking-tight" title={clusterName}>
              {clusterName}
            </span>
            <Badge variant="outline" className="hidden sm:inline-flex">
              Phase 1
            </Badge>
          </div>

          <div className="ml-2 hidden flex-1 max-w-md md:flex">
            <div className="relative w-full">
              <Search
                className="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
                aria-hidden
              />
              <Input
                type="search"
                placeholder="Search (coming in Phase 2)"
                className="pl-8"
                disabled
                aria-label="Global search (disabled)"
              />
            </div>
          </div>

          <div className="ml-auto flex items-center gap-2">
            <ThemeToggle />
            <UserMenu />
          </div>
        </header>

        <main className="flex-1 px-4 py-6 sm:px-6 lg:px-8">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
