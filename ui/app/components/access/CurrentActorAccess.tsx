import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import {
  hasWorkspacePermission,
  loadCurrentActorAccess,
  type CurrentActorAccess,
} from "~/lib/current-actor-access";

type CurrentActorAccessState = {
  access: CurrentActorAccess | null;
  loading: boolean;
  failed: boolean;
};

const CurrentActorAccessContext = createContext<CurrentActorAccessState>({
  access: null,
  loading: false,
  failed: false,
});

export function CurrentActorAccessProvider({ isAuth, children }: { isAuth: boolean; children: ReactNode }) {
  const [state, setState] = useState<CurrentActorAccessState>({ access: null, loading: isAuth, failed: false });

  useEffect(() => {
    let active = true;
    if (!isAuth) {
      setState({ access: null, loading: false, failed: false });
      return () => { active = false; };
    }
    setState({ access: null, loading: true, failed: false });
    loadCurrentActorAccess()
      .then((access) => {
        if (active) setState({ access, loading: false, failed: false });
      })
      .catch(() => {
        if (active) setState({ access: null, loading: false, failed: true });
      });
    return () => { active = false; };
  }, [isAuth]);

  return <CurrentActorAccessContext.Provider value={state}>{children}</CurrentActorAccessContext.Provider>;
}

export function useCurrentActorAccess(): CurrentActorAccessState {
  return useContext(CurrentActorAccessContext);
}

export function WorkspacePermissionGate({ permission, area, children }: { permission: string; area: string; children: ReactNode }) {
  const state = useCurrentActorAccess();
  if (state.loading) return <AccessStatus message="Checking your workspace access…" />;
  if (state.failed) return <AccessStatus message="We couldn't check your workspace access. Refresh the page and try again." alert />;
  if (!hasWorkspacePermission(state.access, permission)) {
    return <AccessDenied area={area} />;
  }
  return <>{children}</>;
}

export function AccessDenied({ area }: { area: string }) {
  return (
    <section className="rounded-xl border border-amber-200 bg-amber-50 p-6" role="status">
      <h1 className="text-lg font-semibold text-amber-950">Access not available</h1>
      <p className="mt-2 text-sm text-amber-900">
        Your account can't view {area}. Ask a workspace administrator if you need access.
      </p>
    </section>
  );
}

function AccessStatus({ message, alert = false }: { message: string; alert?: boolean }) {
  return <p className="rounded-xl border border-slate-200 bg-white p-6 text-sm text-slate-600" role={alert ? "alert" : "status"}>{message}</p>;
}
