import { Trash2 } from "lucide-react";
import { api, type AgentSession } from "~/lib/api";
import { useToast } from "~/components/Toast";

interface IntegrationsPendingTabProps {
  activeSessions: AgentSession[];
  setNewSessionId: (id: string) => void;
  onRefresh: () => void;
}

export default function IntegrationsPendingTab({
  activeSessions,
  setNewSessionId,
  onRefresh
}: IntegrationsPendingTabProps) {
  const toast = useToast();

  return (
    <div className="mb-6 space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-slate-900">Service discovery runs</h2>
          <p className="text-sm text-slate-500 mt-1">
            Review service definitions that are still running or need your attention.
          </p>
        </div>
      </div>
      
      {activeSessions.length === 0 ? (
          <div className="text-center py-16 text-slate-400 bg-white rounded-xl border border-slate-200">
            <p className="text-sm">No active discovery runs</p>
          </div>
      ) : (
        <div className="grid gap-3">
          {activeSessions.map((sess) => (
            <div
              key={sess.id}
              className="w-full text-left flex items-center justify-between p-5 bg-white border border-slate-200 shadow-sm rounded-xl transition-all group overflow-hidden"
            >
              <div className="flex-1 min-w-0 mr-4">
                <div className="flex items-center gap-3">
                  <p className="text-sm font-semibold text-slate-900 truncate">{sess.service_name || "Unnamed Service"}</p>
                  <span className="px-2 py-0.5 bg-amber-100 text-amber-700 text-[10px] font-bold rounded-full tracking-wide shrink-0">
                    {sess.state.toUpperCase().replace("_", " ")}
                  </span>
                </div>
                <div className="flex flex-col mt-2">
                  <p className="text-xs font-medium text-slate-500 truncate">
                    {sess.source_url}
                  </p>
                  <p className="text-[10px] text-slate-400 mt-1">
                    Last Active: {new Date(sess.updated_at).toLocaleString()}
                  </p>
                </div>
              </div>
              <div className="flex items-center gap-3 shrink-0">
                <button
                  data-track="resume_pending_integration"
                  onClick={async () => {
                    try {
                      await api.integrations.recoverSession(sess.id);
                    } catch (e) {
                      console.error("Worker might already be running or failed to recover:", e);
                    }
                    setNewSessionId(sess.id);
                  }}
                  className="px-4 py-2 text-sm font-medium text-blue-600 hover:text-blue-700 bg-blue-50 hover:bg-blue-100 rounded-lg transition-colors cursor-pointer"
                >
                  Resume
                </button>
                <button
                  data-track="delete_pending_integration"
                  onClick={async () => {
                    const confirmed = await toast.confirm("Delete this service discovery run?");
                    if (confirmed) {
                      try {
                        await api.integrations.deleteSession(sess.id);
                        onRefresh(); // Re-fetch active sessions
                        toast.success("Discovery run deleted.");
                      } catch (e) {
                        toast.error(e instanceof Error ? e.message : "Failed to delete session");
                      }
                    }
                  }}
                  className="p-2 text-red-500 hover:text-red-700 bg-red-50 hover:bg-red-100 rounded-lg transition-colors cursor-pointer"
                  title="Delete discovery run"
                >
                  <Trash2 className="w-4 h-4" />
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
