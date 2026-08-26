import { useEffect, useRef, useState } from "react";
import { ExecutionDetails } from "~/components/AnalyticsTab";
import { ExecutionDetailsDrawer } from "~/components/activity/ExecutionDetailsDrawer";
import { UnifiedExecutionDetails } from "~/components/activity/UnifiedExecutionDetails";
import { api, type EngineExecutionEventEntry } from "~/lib/api";
import { unifiedStepRows } from "~/lib/unified-receipt";

interface AppExecutionInspectorProps {
  event: EngineExecutionEventEntry;
  appId: string;
  consumerName: string;
  onClose: () => void;
}

// useUnifiedChildren loads one permission-gated receipt page, never re-executing a target or querying per row.
function useUnifiedChildren(event: EngineExecutionEventEntry, appId: string) {
  const [children, setChildren] = useState<EngineExecutionEventEntry[]>([]);
  const [loading, setLoading] = useState(event.execution_kind === "unified");
  const [unavailable, setUnavailable] = useState(false);
  const [revision, setRevision] = useState(0);

  // One immutable parent owns this request; stale responses cannot replace a newly opened receipt.
  useEffect(() => {
    if (event.execution_kind !== "unified") return;
    let active = true;
    setLoading(true);
    setUnavailable(false);
    api.workspace.listAppExecutionEvents({ appId, includeAllVersions: true, parentExecutionId: event.id, limit: 50 }).then((page) => {
      // Closing the inspector or changing its parent revokes this result's UI ownership.
      if (active) setChildren(page.items);
    }).catch(() => {
      // Query errors remain bounded and cannot surface raw store or authorization diagnostics.
      if (active) setUnavailable(true);
    }).finally(() => {
      // An obsolete request must not clear the current loading indicator.
      if (active) setLoading(false);
    });
    // The underlying API has no abort argument, so cleanup fences both success and failure.
    return () => { active = false; };
  }, [appId, event.execution_kind, event.id, revision]);

  // Refresh only re-reads durable history; it has no provider execution authority.
  const refresh = () => setRevision((value) => value + 1);
  return { rows: unifiedStepRows(event, children), loading, unavailable, refresh };
}

// AppExecutionInspector owns a two-level navigation stack inside the existing scoped receipt drawer.
export function AppExecutionInspector({ event, appId, consumerName, onClose }: AppExecutionInspectorProps) {
  const [child, setChild] = useState<EngineExecutionEventEntry | null>(null);
  const [direction, setDirection] = useState<"forward" | "backward">("forward");
  const scrollRef = useRef<HTMLDivElement>(null);
  const parentScrollTop = useRef(0);
  const receipts = useUnifiedChildren(event, appId);
  // Only physical child selection changes the displayed receipt; the original parent stays mounted in state.
  const selected = child ?? event;

  // Save the parent's viewport before showing an existing provider receipt in the same sidebar.
  const openChild = (receipt: EngineExecutionEventEntry) => {
    parentScrollTop.current = scrollRef.current?.scrollTop ?? 0;
    setDirection("forward");
    setChild(receipt);
  };
  // Back restores both authored ordering and the previous parent scroll position without another request.
  const back = () => {
    setDirection("backward");
    setChild(null);
  };

  return <ExecutionDetailsDrawer event={selected} onClose={onClose} onBack={child ? back : undefined} scrollRef={scrollRef} restoreScrollTop={child ? 0 : parentScrollTop.current} navigationDirection={direction}>
    {/* Parents intentionally omit fabricated service/provider timing; physical children keep the complete canonical renderer. */}
    {selected.execution_kind === "unified" ? <UnifiedExecutionDetails event={event} consumerName={consumerName} rows={receipts.rows} loading={receipts.loading} unavailable={receipts.unavailable} onRetry={receipts.refresh} onSelect={openChild} /> : <>
      {/* The rollback receipt retains its own result and service while stating which target it compensates. */}
      {selected.execution_phase === "rollback" ? <p className="mb-4 rounded-lg border border-slate-200 bg-slate-50 px-3 py-2 text-xs text-slate-600">Rollback execution · compensates <span className="break-words font-mono">{selected.unified_target}</span></p> : null}
      {/* Historical versions still belong to this authorized app family and retain its consumer label. */}
      <ExecutionDetails event={selected} appNames={new Map([[selected.app_id || appId, consumerName]])} />
    </>}
  </ExecutionDetailsDrawer>;
}
