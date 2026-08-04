import { useState, type ComponentType } from "react";
import { Link, useLocation, useNavigate } from "@remix-run/react";
import { Logo } from "~/components/Logo";
import { Layers, Boxes, Bot, KeyRound, ShieldCheck, Activity, Settings, LogOut, ChevronLeft, ChevronRight, Menu, X, LogIn, PlusCircle } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasAnyPermission, hasWorkspacePermission, type CurrentActorAccess } from "~/lib/current-actor-access";

interface IntegrationsSidebarProps {
  isAuth: boolean;
  handleSignOut: () => void;
}

type SidebarIcon = ComponentType<{ className?: string }>;

type SidebarItem = {
  to: string;
  label: string;
  Icon: SidebarIcon;
  // Default active-matching is a pathname prefix check. Creation routes use a
  // custom matcher so the initiating Apps or MCP servers section stays active.
  end?: boolean;
  isActive?: (pathname: string, search: URLSearchParams) => boolean;
  visible?: (access: CurrentActorAccess | null) => boolean;
};

const PUBLIC_NAV_ITEMS: SidebarItem[] = [
  {
    to: "/integrations",
    label: "Services",
    Icon: Layers,
    isActive: (pathname, search) => pathname === "/integrations" && search.get("tab") !== "analytics",
  },
];

const AUTH_NAV_ITEMS: SidebarItem[] = [
  {
    to: "/integrations/sdks",
    label: "Apps",
    Icon: Boxes,
    isActive: (pathname, search) => pathname.startsWith("/integrations/sdks") || (pathname.startsWith("/integrations/sdk-builder") && search.get("tab") !== "mcp"),
    visible: (access) => hasAnyPermission(access, "artifact.read"),
  },
  {
    to: "/integrations/mcp",
    label: "MCP servers",
    Icon: Bot,
    isActive: (pathname, search) => pathname.startsWith("/integrations/mcp") || (pathname.startsWith("/integrations/sdk-builder") && search.get("tab") === "mcp"),
    visible: (access) => hasAnyPermission(access, "artifact.read"),
  },
  {
    to: "/integrations/buckets",
    label: "Credentials",
    Icon: KeyRound,
    visible: (access) => hasAnyPermission(access, "bucket.read"),
  },
  {
    to: "/integrations/access/people",
    label: "Access",
    Icon: ShieldCheck,
    isActive: (pathname) => pathname.startsWith("/integrations/access"),
    visible: (access) => hasWorkspacePermission(access, "access.read"),
  },
  {
    to: "/integrations/observability",
    label: "Activity",
    Icon: Activity,
    visible: (access) => hasAnyPermission(access, "artifact.read"),
  },
  { to: "/integrations/settings", label: "Settings", Icon: Settings, visible: (access) => hasWorkspacePermission(access, "account.read") },
];

export function sidebarItems(isAuth: boolean, access: CurrentActorAccess | null): SidebarItem[] {
  if (!isAuth) return PUBLIC_NAV_ITEMS;
  return [...PUBLIC_NAV_ITEMS, ...AUTH_NAV_ITEMS.filter((item) => item.visible?.(access) ?? true)];
}

function isItemActive(item: SidebarItem, pathname: string, search: URLSearchParams): boolean {
  if (item.isActive) return item.isActive(pathname, search);
  const itemPath = item.to.split("?")[0];
  return item.end ? pathname === itemPath : pathname.startsWith(itemPath);
}

function navClassName(isCollapsed: boolean, isActive: boolean): string {
  const activeClass = isActive
    ? "bg-[var(--brand-violet-tint)] text-[var(--brand-violet)] shadow-sm"
    : "text-slate-600 hover:text-slate-900 hover:bg-slate-100";
  const collapsedClass = isCollapsed ? "justify-center px-2" : "";
  return `flex items-center gap-3 px-3 py-2.5 rounded-lg text-sm font-semibold transition-colors duration-200 ${activeClass} ${collapsedClass}`;
}

export function IntegrationsSidebar({ isAuth, handleSignOut }: IntegrationsSidebarProps) {
  const navigate = useNavigate();
  const [isCollapsed, setIsCollapsed] = useState(false);
  const [isMobileOpen, setIsMobileOpen] = useState(false);
  const { access } = useCurrentActorAccess();

  const closeMobile = () => setIsMobileOpen(false);
  const signIn = () => navigate("/login");
  const signOutFromMobile = () => {
    closeMobile();
    handleSignOut();
  };

  return (
    <>
      <MobileHeader onOpen={() => setIsMobileOpen(true)} />
      <MobileBackdrop isOpen={isMobileOpen} onClose={closeMobile} />
      <MobileDrawer isOpen={isMobileOpen} isAuth={isAuth} access={access} onClose={closeMobile} onSignIn={signIn} onSignOut={signOutFromMobile} />
      <DesktopSidebar isAuth={isAuth} access={access} isCollapsed={isCollapsed} onCollapse={() => setIsCollapsed(true)} onExpand={() => setIsCollapsed(false)} onSignIn={signIn} onSignOut={handleSignOut} />
    </>
  );
}

function MobileHeader({ onOpen }: { onOpen: () => void }) {
  return (
    <header className="lg:hidden flex items-center justify-between px-4 h-14 border-b border-slate-200 bg-white sticky top-0 z-30 w-full shrink-0">
      <div className="flex items-center gap-2">
        <Logo size="sm" logoClassName="w-7 h-7" textClassName="font-extrabold text-slate-900 tracking-tight" />
      </div>
      <button
        data-track="open_mobile_menu"
        aria-label="Open navigation"
        onClick={onOpen}
        className="p-1.5 rounded-lg text-slate-600 hover:bg-slate-100 transition-colors focus:outline-none"
      >
        <Menu className="w-6 h-6" />
      </button>
    </header>
  );
}

function MobileBackdrop({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) {
  if (!isOpen) return null;
  return (
    <div
      className="fixed inset-0 z-40 bg-slate-900/40 backdrop-blur-sm lg:hidden animate-in fade-in duration-200"
      onClick={onClose}
    />
  );
}

function MobileDrawer({ isOpen, isAuth, access, onClose, onSignIn, onSignOut }: { isOpen: boolean; isAuth: boolean; access: CurrentActorAccess | null; onClose: () => void; onSignIn: () => void; onSignOut: () => void }) {
  const drawerClass = isOpen ? "translate-x-0" : "-translate-x-full";
  return (
    <aside className={`fixed inset-y-0 left-0 z-50 w-64 bg-white border-r border-slate-200 flex flex-col transform transition-transform duration-300 ease-in-out lg:hidden ${drawerClass}`}>
      <MobileDrawerHeader onClose={onClose} />
      <nav className="flex-1 px-4 py-6 space-y-1">
        {sidebarItems(isAuth, access).map((item) => (
          <SidebarNavLink key={item.to} item={item} isCollapsed={false} onClick={onClose} />
        ))}
        {!isAuth && <GenerateButton isCollapsed={false} onClick={onSignIn} />}
      </nav>
      <MobileAuthFooter isAuth={isAuth} onSignIn={onSignIn} onSignOut={onSignOut} />
    </aside>
  );
}

function MobileDrawerHeader({ onClose }: { onClose: () => void }) {
  return (
    <div className="h-14 px-6 border-b border-slate-200 flex items-center justify-between">
      <div className="flex items-center gap-2.5">
        <Logo size="sm" logoClassName="w-7 h-7" textClassName="font-extrabold text-slate-900 tracking-tight" />
      </div>
      <button
        data-track="close_mobile_menu"
        aria-label="Close navigation"
        onClick={onClose}
        className="p-1.5 rounded-lg text-slate-500 hover:bg-slate-100 transition-colors"
      >
        <X className="w-5 h-5" />
      </button>
    </div>
  );
}

function MobileAuthFooter({ isAuth, onSignIn, onSignOut }: { isAuth: boolean; onSignIn: () => void; onSignOut: () => void }) {
  return (
    <div className="p-4 border-t border-slate-100">
      {isAuth ? (
        <SignOutButton isCollapsed={false} onClick={onSignOut} />
      ) : (
        <SignInButton isCollapsed={false} onClick={onSignIn} />
      )}
    </div>
  );
}

function DesktopSidebar({ isAuth, access, isCollapsed, onCollapse, onExpand, onSignIn, onSignOut }: { isAuth: boolean; access: CurrentActorAccess | null; isCollapsed: boolean; onCollapse: () => void; onExpand: () => void; onSignIn: () => void; onSignOut: () => void }) {
  const widthClass = isCollapsed ? "w-16" : "w-64";
  return (
    <aside className={`hidden lg:flex flex-col border-r border-slate-200 bg-white sticky top-0 h-screen shrink-0 transition-[width] duration-300 ${widthClass}`}>
      <DesktopHeader isCollapsed={isCollapsed} onCollapse={onCollapse} />
      <DesktopNav isAuth={isAuth} access={access} isCollapsed={isCollapsed} onSignIn={onSignIn} />
      <DesktopFooter isAuth={isAuth} isCollapsed={isCollapsed} onExpand={onExpand} onSignIn={onSignIn} onSignOut={onSignOut} />
    </aside>
  );
}

function DesktopHeader({ isCollapsed, onCollapse }: { isCollapsed: boolean; onCollapse: () => void }) {
  const alignmentClass = isCollapsed ? "justify-center" : "justify-between";
  return (
    <div className={`h-14 border-b border-slate-200 flex items-center px-4 ${alignmentClass}`}>
      <Logo
        size="sm"
        hideText={isCollapsed}
        className="overflow-hidden gap-2.5"
        logoClassName="w-7 h-7 shrink-0"
        textClassName="font-extrabold text-slate-900 tracking-tight animate-in fade-in duration-200"
      />
      {!isCollapsed && (
        <button
          data-track="collapse_sidebar"
          onClick={onCollapse}
          className="p-1 rounded-lg text-slate-400 hover:text-slate-600 hover:bg-slate-100 transition-colors"
        >
          <ChevronLeft className="w-4.5 h-4.5" />
        </button>
      )}
    </div>
  );
}

function DesktopNav({ isAuth, access, isCollapsed, onSignIn }: { isAuth: boolean; access: CurrentActorAccess | null; isCollapsed: boolean; onSignIn: () => void }) {
  return (
    <nav className="flex-1 px-3 py-6 space-y-1">
      {sidebarItems(isAuth, access).map((item) => (
        <SidebarNavLink key={item.to} item={item} isCollapsed={isCollapsed} />
      ))}
      {!isAuth && <GenerateButton isCollapsed={isCollapsed} onClick={onSignIn} />}
    </nav>
  );
}

function DesktopFooter({ isAuth, isCollapsed, onExpand, onSignIn, onSignOut }: { isAuth: boolean; isCollapsed: boolean; onExpand: () => void; onSignIn: () => void; onSignOut: () => void }) {
  return (
    <div className="p-3 border-t border-slate-100 space-y-1">
      {isCollapsed && <ExpandButton onClick={onExpand} />}
      {isAuth ? (
        <SignOutButton isCollapsed={isCollapsed} onClick={onSignOut} />
      ) : (
        <SignInButton isCollapsed={isCollapsed} onClick={onSignIn} />
      )}
    </div>
  );
}

function SidebarNavLink({ item, isCollapsed, onClick }: { item: SidebarItem; isCollapsed: boolean; onClick?: () => void }) {
  const { Icon } = item;
  const location = useLocation();
  const isActive = isItemActive(item, location.pathname, new URLSearchParams(location.search));
  return (
    <Link to={item.to} className={navClassName(isCollapsed, isActive)} onClick={onClick} title={isCollapsed ? item.label : undefined}>
      <Icon className="w-4 h-4 shrink-0" />
      {!isCollapsed && <span className="animate-in fade-in duration-200">{item.label}</span>}
    </Link>
  );
}

function ExpandButton({ onClick }: { onClick: () => void }) {
  return (
    <button
      data-track="expand_sidebar"
      onClick={onClick}
      className="w-full flex items-center justify-center p-2 rounded-lg text-slate-400 hover:text-slate-600 hover:bg-slate-100 transition-colors cursor-pointer mb-2"
      title="Expand sidebar"
    >
      <ChevronRight className="w-4.5 h-4.5" />
    </button>
  );
}

function GenerateButton({ isCollapsed, onClick }: { isCollapsed: boolean; onClick: () => void }) {
  const collapsedClass = isCollapsed ? "justify-center px-2" : "";
  return (
    <button
      data-track="navigate_generate_sdk_mcp"
      onClick={onClick}
      className={`w-full flex items-center gap-3 px-3 py-2.5 rounded-lg text-sm font-semibold text-slate-600 hover:text-slate-900 hover:bg-slate-100 transition-colors duration-200 cursor-pointer ${collapsedClass}`}
      title={isCollapsed ? "Start building" : undefined}
    >
      <PlusCircle className="w-4 h-4 shrink-0" />
      {!isCollapsed && <span className="animate-in fade-in duration-200">Start building</span>}
    </button>
  );
}

function SignOutButton({ isCollapsed, onClick }: { isCollapsed: boolean; onClick: () => void }) {
  return (
    <AuthButton
      isCollapsed={isCollapsed}
      label="Sign out"
      Icon={LogOut}
      colorClass="text-slate-500 hover:text-rose-600 hover:bg-rose-50/50"
      dataTrack="sign_out"
      onClick={onClick}
    />
  );
}

function SignInButton({ isCollapsed, onClick }: { isCollapsed: boolean; onClick: () => void }) {
  return (
    <AuthButton
      isCollapsed={isCollapsed}
      label="Sign in"
      Icon={LogIn}
      colorClass="text-slate-500 hover:text-[var(--brand-violet)] hover:bg-[var(--brand-violet-tint)]"
      dataTrack="sign_in"
      onClick={onClick}
    />
  );
}

function AuthButton({ isCollapsed, label, Icon, colorClass, dataTrack, onClick }: { isCollapsed: boolean; label: string; Icon: SidebarIcon; colorClass: string; dataTrack: string; onClick: () => void }) {
  const collapsedClass = isCollapsed ? "justify-center p-2" : "gap-3 px-3 py-2.5";
  return (
    <button
      data-track={dataTrack}
      onClick={onClick}
      className={`w-full flex items-center rounded-lg text-sm font-medium transition-colors duration-200 cursor-pointer ${colorClass} ${collapsedClass}`}
      title={isCollapsed ? label : undefined}
    >
      <Icon className="w-4 h-4 shrink-0" />
      {!isCollapsed && <span className="animate-in fade-in duration-200">{label}</span>}
    </button>
  );
}
