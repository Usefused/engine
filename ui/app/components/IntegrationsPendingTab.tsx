import { XCircle } from "lucide-react";
import { api, type DiscoverySnapshot } from "~/lib/api";
import { useToast } from "~/components/Toast";
import { cancelDiscoveryAction } from "~/lib/extraction-wizard-protocol";

interface IntegrationsPendingTabProps {
  activeSessions: DiscoverySnapshot[];
  setNewSessionId: (id: string) => void;
  onRefresh: () => void;
}

// IntegrationsPendingTab lists resumable version-one snapshots without reconstructing removed session fields.
export default function IntegrationsPendingTab({ activeSessions, setNewSessionId, onRefresh }: IntegrationsPendingTabProps) {
  const toast = useToast();

  // cancelDiscoveryRun uses the typed revision-bound action and leaves the durable audit intact.
  async function cancelDiscoveryRun(session: DiscoverySnapshot) {
    const confirmed = await toast.confirm("Cancel this service discovery run?");
    if (!confirmed) return;
    try {
      await api.integrations.actOnDiscovery(session.session_id, cancelDiscoveryAction(session));
      onRefresh();
      toast.success("Discovery run cancelled.");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Failed to cancel discovery run");
    }
  }

  return (
    <div className="mb-6 space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-slate-900">Service discovery runs</h2>
        <p className="text-sm text-slate-500 mt-1">Review service definitions that are still running or need your attention.</p>
      </div>
      {activeSessions.length === 0 ? (
        <div className="text-center py-16 text-slate-400 bg-white rounded-xl border border-slate-200"><p className="text-sm">No active discovery runs</p></div>
      ) : (
        <div className="grid gap-3">
          {activeSessions.map((session) => (
            <div key={session.session_id} className="w-full text-left flex items-center justify-between p-5 bg-white border border-slate-200 shadow-sm rounded-xl group overflow-hidden">
              <div className="flex-1 min-w-0 mr-4">
                <div className="flex items-center gap-3">
                  <p className="text-sm font-semibold text-slate-900 truncate">Discovery {session.session_id.slice(0, 8)}</p>
                  <span className="px-2 py-0.5 bg-amber-100 text-amber-700 text-[10px] font-bold rounded-full tracking-wide shrink-0">{session.state.toUpperCase().replaceAll("_", " ")}</span>
                </div>
                <div className="flex flex-col mt-2 text-xs text-slate-500">
                  <p>Revision {session.revision}{session.draft_revision ? ` · Draft ${session.draft_revision}` : ""}</p>
                  <p className="mt-1">{session.payload?.operations?.length || 0} operations · {session.payload?.effective_workers || 0} workers</p>
                </div>
              </div>
              <div className="flex items-center gap-3 shrink-0">
                <button data-track="resume_pending_integration" onClick={() => setNewSessionId(session.session_id)} className="px-4 py-2 text-sm font-medium text-blue-600 hover:text-blue-700 bg-blue-50 hover:bg-blue-100 rounded-lg cursor-pointer">Resume</button>
                <button data-track="cancel_pending_integration" onClick={() => cancelDiscoveryRun(session)} className="p-2 text-red-500 hover:text-red-700 bg-red-50 hover:bg-red-100 rounded-lg cursor-pointer" title="Cancel discovery run"><XCircle className="w-4 h-4" /></button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
