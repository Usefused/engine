import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { useLocation, useNavigate } from "@remix-run/react";
import { Sparkles } from "lucide-react";
import { FusedAgentContext, type FusedDraftBuilderBridge, type FusedEditorBridge } from "./FusedAgentContext";
import { FusedAgentPage, fusedValueHidden, fusedPagePath } from "~/lib/fused-agent-page";
import { harnest, messageText, type HarnestApproval, type HarnestClientTool, type HarnestStreamEvent } from "~/lib/fused-agent-transport";
import { searchAgentServices, searchAgentOperations, readAgentServiceContract } from "~/lib/fused-agent-discovery";
import { readAgentContracts } from "~/lib/fused-agent-contracts";

import { agentInput, visibleUserRequest } from "./agent-input";
import { updateToolActivity } from "./tool-activity";
import FusedAgentChat from "./FusedAgentChat";
import type { FusedChatMessage as Message } from "./fused-chat-types";

/** Preserves one conversation while yielding desktop sidebar space to workspace detail drawers. */
export function FusedAgentProvider({ children, authenticated }: { children: ReactNode; authenticated: boolean }) {
  const navigate = useNavigate();
  const location = useLocation();
  const [open, setOpen] = useState(false);
  const [detailSidebarOpen, setDetailSidebarOpen] = useState(false);
  const [prompt, setPrompt] = useState("");
  const [messages, setMessages] = useState<Message[]>([]);
  const [status, setStatus] = useState("loading");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [includePage, setIncludePage] = useState(true);
  const connected = useRef(true);
  connected.current = includePage;
  const session = useRef<string | null>(null);
  const run = useRef<AbortController | null>(null);
  const page = useRef(new FusedAgentPage());
  const editor = useRef<FusedEditorBridge | null>(null);
  const draftBuilder = useRef<FusedDraftBuilderBridge | null>(null);
  const route = useRef(location.pathname + location.search);
  route.current = location.pathname + location.search;
  const historyRequest = useRef<AbortController | null>(null);
  const [loadingHistory, setLoadingHistory] = useState(false);
  const [statusRevision, setStatusRevision] = useState(0);
  const workspacePane = useRef<HTMLDivElement>(null);
  const [approval, setApproval] = useState<HarnestApproval | null>(null);
  const approvalDecision = useRef<((decision: 'approve' | 'deny') => void) | null>(null);

  // Unmounting cannot leave an approved task waiting to edit a detached page.
  useEffect(() => () => { run.current?.abort(); }, []);

  /** Suspends the exact runtime call until an explicit decision, cancellation, or expiry. */
  function requestApproval(value: HarnestApproval, signal: AbortSignal): Promise<'approve' | 'deny'> {
    signal.throwIfAborted();
    return new Promise((resolve) => {
      let settled = false;
      /** Consumes the UI decision once and removes every cancellation hook. */
      function finish(decision: 'approve' | 'deny') {
        // Double clicks, stop, and expiry cannot grant a second execution.
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        signal.removeEventListener('abort', cancel);
        approvalDecision.current = null;
        setApproval(null);
        // An expired prompt cannot be approved even before the runtime checks it.
        resolve(Date.parse(value.expiresAt) > Date.now() ? decision : 'deny');
      }
      /** Stopping a response never implies consent to its pending action. */
      function cancel() { finish('deny'); }
      const timer = setTimeout(cancel, Math.max(0, Date.parse(value.expiresAt) - Date.now()));
      approvalDecision.current = finish;
      setApproval(value);
      signal.addEventListener('abort', cancel, { once: true });
    });
  }

  /** Only a click for the currently displayed approval may release its suspended call. */
  function decideApproval(id: string, decision: 'approve' | 'deny') {
    // Old prompts must not approve a later action after a rerender.
    if (approval?.id === id) approvalDecision.current?.(decision);
  }

  // Explicit drawer markers avoid mistaking navigation, popovers, or assistant content for another sidebar.
  useEffect(() => {
    const workspace = workspacePane.current;
    // The provider may unmount before the workspace ref becomes available.
    if (!workspace) return;
    /** Tracks mounted detail drawers without coupling conversation state to individual routes. */
    function updateSidebarPresence() {
      setDetailSidebarOpen(Boolean(workspace?.querySelector("[data-fused-detail-sidebar]")));
    }
    updateSidebarPresence();
    const observer = new MutationObserver(updateSidebarPresence);
    observer.observe(workspace, { childList: true, subtree: true });
    return () => { observer.disconnect(); };
  }, []);

  // Mobile chat fills the viewport, but its mounted page remains available to explicitly enabled page tools.
  useEffect(() => {
    const compact = window.matchMedia("(max-width: 767px)");
    /** Prevents keyboard focus reaching the collapsed mobile page while desktop retains two usable panes. */
    function updateWorkspaceFocus() {
      // The ref can disappear during unmount; no detached page should retain an interaction lock.
      if (workspacePane.current) workspacePane.current.inert = authenticated && open && compact.matches;
    }
    updateWorkspaceFocus();
    compact.addEventListener("change", updateWorkspaceFocus);
    return () => { compact.removeEventListener("change", updateWorkspaceFocus); };
  }, [authenticated, open]);

  /** Registers only the active editor; stale unmounts cannot remove its replacement. */
  const registerEditor = useCallback((bridge: FusedEditorBridge) => {
    editor.current = bridge;
    return () => { /* A replaced editor owns its own cleanup. */ if (editor.current === bridge) editor.current = null; };
  }, []);
  /** Registers only the active creation page, including before it has a source editor. */
  const registerDraftBuilder = useCallback((bridge: FusedDraftBuilderBridge) => {
    draftBuilder.current = bridge;
    return () => { /* A newer page registration must survive the old page's cleanup. */ if (draftBuilder.current === bridge) draftBuilder.current = null; };
  }, []);
  /** Launches the shared sidebar from any page-specific entry point. */
  const launch = useCallback((message?: string) => { setOpen(true); /* Optional launcher text stays editable before sending. */ if (message) setPrompt(message); }, []);

  // Signing out clears model context; closing the sidebar does not abandon an active reply.
  useEffect(() => {
    // Unauthenticated pages cannot retain a previous actor's conversation.
    if (!authenticated) { run.current?.abort(); historyRequest.current?.abort(); session.current = null; setMessages([]); setOpen(false); }
    return () => { run.current?.abort(); historyRequest.current?.abort(); };
  }, [authenticated]);
  // Startup is asynchronous; refresh status while the installed runtime is becoming ready.
  useEffect(() => {
    // Status is only needed when an authenticated user opens the assistant.
    if (!authenticated || !open) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    /** Polls startup without retaining a timer after closing or signing out. */
    async function check() {
      try {
        const response = await fetch("/agent/status", { credentials: "include", signal: controller.signal });
        // HTTP failures must not masquerade as an enabled runtime.
        if (!response.ok) throw new Error("Agent status unavailable");
        const result = await response.json();
        // A cancelled status request cannot update another page's state.
        if (controller.signal.aborted) return;
        setStatus(result.status);
        // Only startup needs polling; configuration failures remain actionable rather than retrying forever.
        if (result.status === "starting") timer = setTimeout(check, 2000);
      } catch { if (!controller.signal.aborted) setStatus("unavailable"); }
    }
    void check();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [authenticated, open, statusRevision]);

  /** Reads the active workspace dialog first, keeping body portals in scope without exposing unrelated DOM. */
  function snapshot() {
    // Context opt-out applies equally to page content and portaled drafts.
    if (!connected.current) throw new Error("Page context is disabled. Ask the user to enable it.");
    // Only explicitly marked workspace dialogs may replace the normal page context.
    const root = document.querySelector<HTMLElement>("[data-fused-workspace-dialog]") ?? document.querySelector<HTMLElement>("[data-fused-workspace]");
    // Detached routes cannot provide fields for assistant edits.
    if (!root) throw new Error("No workspace page is connected.");
    return { root, context: page.current.snapshot(root, window.location) };
  }

  /** Executes only declared draft tools; there is no arbitrary selector, script, API, or submit capability. */
  async function executeTool(call: HarnestClientTool, signal: AbortSignal): Promise<unknown> {
    signal.throwIfAborted();
    try {
      const { root, context } = snapshot();
      const args = call.arguments;
      // Catalogue discovery uses the caller's existing Registry permissions and never mutates workspace state.
      if (call.name === "search_services") return await searchAgentServices(String(args.query ?? ""), Number(args.offset ?? 0));
      // Explicit version resolution prevents endpoint discovery from widening an existing app's contract.
      if (call.name === "search_service_operations") return await searchAgentOperations(String(args.service_id ?? ""), String(args.version ?? ""), String(args.query ?? ""), Number(args.offset ?? 0));
      // Reading a discovered contract is independent of selecting or running it.
      if (call.name === "read_service_contract") return await readAgentServiceContract(String(args.service_id ?? ""), String(args.version ?? ""), String(args.operation ?? ""), String(args.path ?? ""), Number(args.offset ?? 0));
      const source = editor.current && document.getElementById(editor.current.fieldID);
      const bridge = source && !fusedValueHidden(source) && source.getClientRects().length > 0 ? editor.current : null;
      if (call.name === "get_page_context") return { ...context, canDescribeUnifiedApp: Boolean(draftBuilder.current?.available), unifiedApp: bridge ? { services: bridge.services, view: bridge.view, configError: bridge.configError, canCompile: bridge.available, canRevise: bridge.available && bridge.view === "typescript" } : undefined };
      // The agent switches ordinary non-secret editor tabs instead of reading hidden fields through another channel.
      if (call.name === "set_unified_app_view") {
        // Only the connected authorized editor can switch; stale context must not target a different draft.
        if (!bridge || Number(args.expected_revision) !== context.revision || !["typescript", "yaml"].includes(String(args.view))) throw new Error("Read the connected app editor and choose typescript or yaml.");
        // Re-selecting the current tab is harmless, including while repairing invalid YAML.
        if (args.view !== bridge.view) bridge.setView(args.view as "typescript" | "yaml");
        await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
        return { view: editor.current?.view, page: snapshot().context, saved: false };
      }
      // Revisions use Describe's discovery and source APIs within the current authorized editor.
      if (call.name === "revise_unified_app") {
        // Stale context and incomplete forms cannot replace source or selected capabilities.
        if (!bridge?.available || bridge.view !== "typescript" || Number(args.expected_revision) !== context.revision || typeof args.goal !== "string" || !args.goal.trim() || new TextEncoder().encode(args.goal).length > 16384) throw new Error("Read the current ready TypeScript draft and provide a change of 1 to 16,384 bytes.");
        const result = await bridge.revise(args.goal, signal);
        // The next context read must observe both React updates, not the previous selection.
        await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
        return result;
      }
      // Description uses the current form's existing API flow and produces only an unsaved draft.
      if (call.name === "describe_unified_app") {
        // Reject detached, busy, or oversized requests before invoking the authorized page workflow.
        if (!draftBuilder.current?.available || typeof args.goal !== "string" || !args.goal.trim() || new TextEncoder().encode(args.goal).length > 16384) throw new Error("Open an available Unified App creation page and provide a goal of 1 to 16,384 bytes.");
        return await draftBuilder.current.describe(args.goal, signal);
      }
      if (call.name === "update_form_field") {
        page.current.update(root, window.location, Number(args.expected_revision), String(args.field_id), args.value as string | boolean);
        // Let controlled React fields commit before reporting the actual draft value.
        await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
        const updated = page.current.snapshot(root, window.location);
        // Controlled fields may reject an edit; report the committed value rather than claiming a change.
        if (updated.fields.find((field) => field.id === args.field_id)?.value !== args.value) throw new Error("The form did not accept that value.");
        // YAML syntax and config validation can reject a raw edit even though the text field accepted it.
        if (editor.current?.view === "yaml" && editor.current.configError) return { ok: false, error: editor.current.configError, page: updated, saved: false };
        return { updated: true, page: updated, saved: false };
      }
      if (call.name === "navigate_ui") {
        if (typeof args.path !== "string" || !context.links.some((link) => link.path === args.path)) throw new Error("Choose a path from the current page's links.");
        navigate(args.path);
        // Navigation must commit before the next tool reads fields from the destination page.
        for (let attempt = 0; route.current !== args.path && attempt < 100; attempt++) {
          signal.throwIfAborted();
          await new Promise<void>((resolve) => setTimeout(resolve, 20));
        }
        // Uncompleted navigation must not be mistaken for a new editor context.
        if (route.current !== args.path) throw new Error("Navigation has not completed. Read the page again.");
        // Page data often loads after route commit; require a fresh read rather than reporting transient empty lists.
        return { navigated: true, path: route.current, next: "Read the destination with get_page_context before describing its contents. Re-read if it is still loading." };
      }
      if (call.name === "validate_form") return { errors: Array.from(root.querySelectorAll<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>("input,textarea,select")).filter((field) => !fusedValueHidden(field) && !field.closest("[data-fused-agent]") && !field.checkValidity()).map((field) => ({ field: field.name || field.id, message: field.validationMessage })), submitted: false };
      if (call.name === "read_selected_contracts") {
        if (!bridge) throw new Error("Open a Unified App editor to inspect its selected contracts.");
        return await readAgentContracts(bridge.services, typeof args.path === "string" ? args.path : "", Number(args.offset ?? 0));
      }
      if (call.name === "compile_unified_app") {
        if (!bridge?.available || Number(args.expected_revision) !== context.revision) throw new Error("Read the current editable app draft before compiling it.");
        return await bridge.compile();
      }
      throw new Error("Unsupported Fused tool.");
    } catch (cause) { signal.throwIfAborted(); return { ok: false, error: cause instanceof Error ? cause.message : "The tool failed." }; }
  }

  /** Sends a follow-up in the same Harnest session and resumes each authenticated frontend-tool request. */
  async function send() {
    // Restore must finish before a turn can select its destination session.
    if (!prompt.trim() || run.current || loadingHistory || status !== "ready") return;
    const text = prompt.trim(); const id = crypto.randomUUID(); const controller = new AbortController();
    run.current = controller; setBusy(true); setError(""); setPrompt("");
    setMessages((previous) => [...previous, { id: crypto.randomUUID(), role: "user", text }, { id, role: "assistant", text: "", tools: [] }]);
    /** Updates only this response so late completions cannot change another conversation. */
    function update(change: (message: Message) => Message) { if (!controller.signal.aborted) setMessages((previous) => previous.map((message) => message.id === id ? change(message) : message)); }
    /** Streams text and status without rendering model text as HTML. */
    function event(value: HarnestStreamEvent) {
      // Server-side skill discovery uses the same activity cards as frontend draft tools.
      if (value.type === "response.tool_call" && value.name) update(message => ({ ...message, tools: updateToolActivity(message.tools, { id: value.id || `${value.name}-${value.sequence}`, name: value.name!, status: "running" }) }));
      // Match the exact streamed call where available; names alone can repeat within one turn.
      if (value.type === "response.tool_result" && value.name) update(message => ({ ...message, tools: updateToolActivity(message.tools, { id: value.callId || value.id || `${value.name}-${value.sequence}`, name: value.name!, status: "completed" }, true) }));
      // Text deltas append to this response while completion remains authoritative.
      if (value.type === "response.text.delta") update((message) => ({ ...message, text: message.text + (value.delta ?? "") }));
      // JSON continuations omit incremental server-tool results; authoritative completion settles their cards.
      if (value.type === "response.completed" && value.status === "completed") update((message) => ({ ...message, text: value.outputText || message.text, tools: message.tools?.map(tool => tool.status === 'running' ? { ...tool, status: 'completed' } : tool) }));
      if (value.type === "error") throw new Error(value.error || "Fused could not complete this response.");
    }
    try {
      const activeSession = session.current ?? (await harnest.createSession(text.slice(0, 70), controller.signal)).id;
      // Reset or stop may occur while creation is pending; an abandoned turn cannot install its session afterward.
      controller.signal.throwIfAborted();
      session.current = activeSession;
      await harnest.streamResponse(agentInput(text, connected.current ? fusedPagePath(window.location) : undefined), session.current, event, controller.signal, async (call) => {
        update((message) => ({ ...message, tools: updateToolActivity(message.tools, { id: call.id, name: call.name, status: "running" }, true) }));
        const result = await executeTool(call, controller.signal);
        controller.signal.throwIfAborted();
        // Failed draft actions stay visibly failed instead of receiving a success check mark.
        update((message) => ({ ...message, tools: message.tools?.map((tool) => tool.id === call.id ? { ...tool, status: result && typeof result === "object" && "ok" in result && result.ok === false ? "failed" : "completed" } : tool) }));
        return result;
      }, { path: connected.current ? fusedPagePath(window.location) : undefined, includePageContext: connected.current }, value => requestApproval(value, controller.signal));
    } catch (cause) {
      // A failed turn must settle running cards so a retry cannot look like concurrent unfinished work.
      if (!controller.signal.aborted) { session.current = null; setError(cause instanceof Error ? cause.message : "The agent could not complete this request."); update(message => ({ ...message, tools: message.tools?.map(tool => tool.status === 'running' ? { ...tool, status: 'failed' } : tool) })); }
    } finally { if (run.current === controller) { run.current = null; setBusy(false); } }
  }

  /** Cancels suspended work so a later message cannot execute an abandoned client action. */
  function stop() {
    run.current?.abort(); run.current = null; session.current = null; setBusy(false);
    // Interrupted tool cards cannot remain in a perpetual working state.
    setMessages(previous => previous.map(message => ({ ...message, text: message.role === 'assistant' && !message.text ? 'Response stopped.' : message.text, tools: message.tools?.map(tool => tool.status === 'running' ? { ...tool, status: 'stopped' } : tool) })));
  }
  /** Clears conversational state without changing any page draft. */
  function reset() { stop(); historyRequest.current?.abort(); setLoadingHistory(false); setMessages([]); setError(""); setPrompt(""); }

  /** Rechecks runtime readiness without clearing the current conversation or draft. */
  function refreshStatus() { setStatus("loading"); setStatusRevision(previous => previous + 1); }
  /** Restores a server-owned session without allowing a late fetch to overwrite a newer conversation. */
  async function openSession(id: string): Promise<boolean> {
    // In-flight tools belong to the current session and must not run against a newly selected one.
    if (run.current) return false;
    historyRequest.current?.abort();
    const controller = new AbortController(); historyRequest.current = controller;
    setLoadingHistory(true); setError("");
    try {
      const history = await harnest.getMessages(id, controller.signal);
      // Reset, sign-out or a newer selection invalidates this restore.
      if (controller.signal.aborted) return false;
      const restored: Message[] = history.flatMap(message => {
        // Internal tool envelopes are not conversational messages.
        if (message.role !== 'user' && message.role !== 'assistant') return [];
        const content = messageText(message.content);
        // Route envelopes are transport context, not user-authored conversation copy.
        const text = message.role === 'user' ? visibleUserRequest(content) : content;
        // Empty tool-call envelopes must not appear as unfinished assistant replies.
        if (!text.trim()) return [];
        return [{ id: message.id, role: message.role, text }];
      });
      session.current = id; setMessages(restored); return true;
    } catch (cause) {
      // Abandoned requests are expected during navigation and do not indicate a transport failure.
      if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not restore conversation.");
      return false;
    } finally { /* Only the current request owns the loading indicator. */ if (historyRequest.current === controller) setLoadingHistory(false); }
  }
  /** Deletes actor-owned history only after the conversation chooser's explicit confirmation. */
  async function deleteSession(id: string) {
    // Active responses must finish or be stopped before their history can be removed.
    if (run.current) throw new Error("Stop the current response before deleting a conversation.");
    await harnest.deleteSession(id);
    // Deleting another conversation must not reset the currently visible one.
    if (session.current === id) reset();
  }

  /** Opens from the icon without treating its click event as a suggested prompt. */
  function openSidebar() { launch(); }
  /** Restores workspace space while preserving the conversation and in-progress response. */
  function closeSidebar() { setOpen(false); }

  return <FusedAgentContext.Provider value={{ isOpen: open, open: launch, registerEditor, registerDraftBuilder }}>
    {/* Desktop panes scroll independently; mobile chat covers the inert page without losing its draft. */}
    <div className="flex h-dvh min-w-0 flex-col overflow-hidden md:flex-row">
    {/* Keep fixed drawers anchored to the viewport; transforming this scroller would clip them after page scrolling. */}
    <div ref={workspacePane} data-fused-workspace-pane className="isolate min-h-0 min-w-0 flex-1 overflow-auto">{children}</div>
    {/* Authentication gates assistant access; a closed pane releases all of its layout space. */}
    {authenticated && <div data-fused-agent className="shrink-0">
      {/* The icon stays discoverable without competing with primary page actions. */}
      {!open && <button type="button" onClick={openSidebar} aria-label="Ask Fused" title="Ask Fused" aria-controls="fused-assistant" aria-expanded={false} className="fixed bottom-5 right-5 z-40 flex h-12 w-12 items-center justify-center rounded-full border border-violet-200 bg-white text-violet-600 shadow-md transition-colors hover:border-violet-300 hover:bg-violet-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-violet-500"><Sparkles className="h-5 w-5" strokeWidth={1.6} aria-hidden="true" /></button>}
      {/* The copied chat renderer shares the existing provider so popup transitions preserve active tools and drafts. */}
      {open && <FusedAgentChat popup={detailSidebarOpen} pageTitle={typeof document === 'undefined' ? 'Workspace' : document.title.replace(/\s*-\s*Fused$/, '')}
        status={status} messages={messages} composer={prompt} setComposer={setPrompt} includeContext={includePage} setIncludeContext={setIncludePage}
        isSending={busy} loadingHistory={loadingHistory} error={error} sessionId={session.current} sendMessage={send} newConversation={reset}
        approval={approval} decideApproval={decideApproval}
        closeAgent={closeSidebar} stop={stop} refreshAgentStatus={refreshStatus} openSession={openSession} deleteSession={deleteSession} />}

    </div>}
    </div>
  </FusedAgentContext.Provider>;
}
