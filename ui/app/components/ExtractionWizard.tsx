import { useEffect, useState, type Dispatch, type FormEvent, type SetStateAction } from "react";
import { useNavigate } from "@remix-run/react";
import { fetchEventSource } from "@microsoft/fetch-event-source";
import { Loader2, Server, PlayCircle } from "lucide-react";
import { api, type IntegrationObject, BASE, handleCredentialedResponse } from "~/lib/api";
import EndpointSelectionList from "~/components/EndpointSelectionList";
import WebhookSelectionList, { type SelectableEvent } from "~/components/WebhookSelectionList";
import { useToast } from "~/components/Toast";

const DEFAULT_AGENT_MAX_IMPORT_SELECTIONS = 20;

function getMaxAgentImportSelections() {
  const raw = typeof window !== "undefined"
    ? window.ENV?.AGENT_MAX_IMPORT_SELECTIONS
    : undefined;
  const parsed = Number.parseInt(String(raw || ""), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_AGENT_MAX_IMPORT_SELECTIONS;
}

interface QuestionData {
  id: string;
  text: string;
  endpoints?: IntegrationObject[];
  default_value?: string;
}

interface WizardEvent {
  type: "thinking" | "awaiting_input" | "extraction_started" | "complete" | "error";
  message?: string;
  questions?: QuestionData[];
  integration_id?: string;
  version?: string;
}

interface WizardEventActions {
  setLoading: Dispatch<SetStateAction<boolean>>;
  setLoadingMessage: Dispatch<SetStateAction<string>>;
  setError: Dispatch<SetStateAction<string>>;
  setQuestion: Dispatch<SetStateAction<QuestionData | null>>;
  setSelectedEndpoints: Dispatch<SetStateAction<Set<string>>>;
  setSubmitting: Dispatch<SetStateAction<boolean>>;
  navigate: (path: string) => void;
  notifyExtractionStarted: () => void;
  finish: () => void;
  sessionId: string;
}

function endpointSelectionIds(question: QuestionData): Set<string> {
  return new Set((question.endpoints || []).map(endpoint => `${endpoint.method}|${endpoint.path}|${endpoint.name || ""}`));
}

function applyQuestion(questions: QuestionData[] | undefined, actions: WizardEventActions) {
  const question = questions?.[0];
  if (!question) return;
  actions.setQuestion(question);
  actions.setSelectedEndpoints(endpointSelectionIds(question));
}

async function completeWizardEvent(event: WizardEvent, actions: WizardEventActions) {
  actions.setLoading(false);
  if (!event.integration_id) return;
  try {
    await api.workspace.addService(event.integration_id, "", event.version || "");
  } catch (error) {
    actions.setError(error instanceof Error ? error.message : "Workspace auto-registration failed.");
    return;
  }
  actions.navigate(`/integrations/${event.integration_id}`);
  actions.finish();
}

async function handleWizardEvent(event: WizardEvent, actions: WizardEventActions) {
  switch (event.type) {
    case "thinking":
      actions.setLoading(true);
      actions.setLoadingMessage(event.message || "Thinking...");
      return;
    case "awaiting_input":
      actions.setLoading(false);
      applyQuestion(event.questions, actions);
      return;
    case "extraction_started":
      actions.notifyExtractionStarted();
      actions.setLoading(true);
      actions.setLoadingMessage(event.message || "Finding selected operations...");
      return;
    case "complete":
      await completeWizardEvent(event, actions);
      return;
    case "error":
      actions.setLoading(false);
      actions.setError(event.message || "An error occurred");
      actions.setSubmitting(false);
  }
}

function parsePendingQuestion(raw: string | undefined): QuestionData[] | undefined {
  if (!raw) return undefined;
  try {
    return JSON.parse(raw) as QuestionData[];
  } catch (error) {
    console.error("Failed to parse pending question", error);
    return undefined;
  }
}

export default function ExtractionWizard({ 
  sessionId, 
  onClose, 
  onComplete,
}: { 
  sessionId: string; 
  onClose: () => void; 
  onComplete?: () => void;
}) {
  const navigate = useNavigate();
  const toast = useToast();
  
  const [loading, setLoading] = useState(true);
  const [loadingMessage, setLoadingMessage] = useState("Initializing extraction wizard...");
  const [error, setError] = useState("");
  
  const [targetType, setTargetType] = useState<string>("endpoints");
  const [question, setQuestion] = useState<QuestionData | null>(null);
  const [selectedEndpoints, setSelectedEndpoints] = useState<Set<string>>(new Set());
  const [submitting, setSubmitting] = useState(false);
  const maxAgentImportSelections = getMaxAgentImportSelections();

  useEffect(() => {
    if (!sessionId) return;
    
		const ctrl = new AbortController();
    let retryDelay = 2000;
    const maxRetryDelay = 60000;

    async function init() {
      try {
        const sess = await api.integrations.getSession(sessionId!);
        if (ctrl.signal.aborted) return;

        if (sess) applyLoadedSession(sess);
      } catch (err) {
        if (ctrl.signal.aborted) return;
        console.error("Failed to load session history", err);
      }

      if (ctrl.signal.aborted) return;

      // Connect to the live stream
		fetchEventSource(`${BASE}/integrations/session/${sessionId}/stream`, {
			credentials: "include",
        signal: ctrl.signal,
        async onopen(response) {
          handleCredentialedResponse(response);
          if (response.status === 401) ctrl.abort();
          if (response.ok) {
            setError("");
            retryDelay = 2000; // reset delay on successful connection
          } else {
            throw new Error(`Failed to connect: ${response.status}`);
          }
        },
        async onmessage(ev) {
          try {
            setError("");
            await handleWizardEvent(JSON.parse(ev.data) as WizardEvent, eventActions);
          } catch {
            console.error("Failed to parse SSE event", ev.data);
          }
        },
        onerror(err) {
          if (ctrl.signal.aborted) {
            throw err;
          }
          console.warn(`SSE connection lost. Reconnecting in ${retryDelay}ms...`, err);
          const delay = retryDelay;
          retryDelay = Math.min(retryDelay * 2, maxRetryDelay);
          return delay;
        }
      });
    }

    init();

    return () => {
      ctrl.abort();
    };
  }, [sessionId]);

  function finishWizard() {
    if (onComplete) onComplete(); else onClose();
  }

  const eventActions: WizardEventActions = {
    setLoading,
    setLoadingMessage,
    setError,
    setQuestion,
    setSelectedEndpoints,
    setSubmitting,
    navigate,
    notifyExtractionStarted: () => toast.success("Operation discovery started."),
    finish: finishWizard,
    sessionId,
  };

  function applyLoadedSession(sess: Awaited<ReturnType<typeof api.integrations.getSession>>) {
    setTargetType(sess.target_type || "endpoints");
    if (sess.state === "error") {
      setLoading(false);
      setError(sess.error || "The agent encountered a fatal error and stopped.");
      return;
    }
    applyQuestion(parsePendingQuestion(sess.pending_question), eventActions);
  }

  async function handleSubmitAnswer(e: FormEvent) {
    e.preventDefault();
    if (!question || submitting) return;
    if (selectedEndpoints.size > maxAgentImportSelections) {
      setError(`Select ${maxAgentImportSelections} or fewer endpoints per import batch.`);
      return;
    }
    
    setSubmitting(true);
    const allSelected: { method: string, path: string, name?: string }[] = [];
    
    if (question.endpoints) {
      question.endpoints.forEach(ep => {
        if (selectedEndpoints.has(`${ep.method}|${ep.path}|${ep.name || ""}`)) {
          allSelected.push({ method: ep.method, path: ep.path, name: ep.name });
        }
      });
    }
    
    const answersMap: Record<string, string> = {
      "preview": JSON.stringify({ selected_items: allSelected })
    };
    
    const answerString = JSON.stringify(answersMap);

    setLoading(true);
    setLoadingMessage("Dispatching extraction workers...");
    setError("");

    try {
      await api.integrations.respond(sessionId, answerString, undefined);
    } catch (error) {
      setError(error instanceof Error ? error.message : "Failed to start extraction");
      setLoading(false);
      setSubmitting(false);
    }
  }

  return (
    <ExtractionWizardView
      sessionId={sessionId}
      onClose={onClose}
      loading={loading}
      loadingMessage={loadingMessage}
      error={error}
      question={question}
      targetType={targetType}
      selectedEndpoints={selectedEndpoints}
      setSelectedEndpoints={setSelectedEndpoints}
      submitting={submitting}
      maxSelections={maxAgentImportSelections}
      onSubmit={handleSubmitAnswer}
      toast={toast}
    />
  );
}

interface ExtractionWizardViewProps {
  sessionId: string;
  onClose: () => void;
  loading: boolean;
  loadingMessage: string;
  error: string;
  question: QuestionData | null;
  targetType: string;
  selectedEndpoints: Set<string>;
  setSelectedEndpoints: Dispatch<SetStateAction<Set<string>>>;
  submitting: boolean;
  maxSelections: number;
  onSubmit: (event: FormEvent) => void;
  toast: ReturnType<typeof useToast>;
}

function ExtractionWizardView(props: ExtractionWizardViewProps) {
  return (
    <>
      <div className="fixed inset-0 bg-slate-900/40 backdrop-blur-sm z-40 transition-opacity" onClick={() => closeOverlay(props)} />
      <div className="fixed inset-y-0 right-0 w-full md:w-[600px] lg:w-[700px] bg-slate-50 shadow-2xl z-50 overflow-y-auto transform transition-transform border-l border-slate-200 flex flex-col animate-in slide-in-from-right duration-300">
        <WizardHeader {...props} />
        <div className="flex-1 flex flex-col p-6">
          <WizardError error={props.error} />
          <WizardContent {...props} />
        </div>
      </div>
    </>
  );
}

function closeOverlay(props: ExtractionWizardViewProps) {
  if (!props.submitting) props.onClose();
}

function WizardHeader(props: ExtractionWizardViewProps) {
  return (
    <div className="p-6 border-b border-slate-200 flex items-center justify-between sticky top-0 bg-slate-50/90 backdrop-blur z-10">
      <div>
        <div className="flex items-center gap-2">
          <h1 className="text-xl font-bold text-slate-900 tracking-tight">Review discovered operations</h1>
          <span className="inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-semibold bg-[var(--brand-violet-tint)] text-[var(--brand-violet)] border border-[var(--brand-violet)]/20 uppercase tracking-wider">Automated</span>
        </div>
        <p className="text-xs text-slate-500 mt-1">Choose the operations this service should expose.</p>
      </div>
      <button data-track="cancel_extraction" onClick={() => cancelExtraction(props)} className="px-3 py-1.5 text-xs font-medium text-slate-500 hover:text-red-600 hover:bg-red-50 rounded-md transition-colors cursor-pointer border border-transparent">
        Cancel discovery
      </button>
    </div>
  );
}

async function cancelExtraction(props: ExtractionWizardViewProps) {
  const confirmed = await props.toast.confirm("Cancel this service discovery run?");
  if (!confirmed) return;
  api.integrations.cancelSession(props.sessionId).catch(console.error);
  props.toast.success("Discovery cancelled.");
  props.onClose();
}

function WizardError({ error }: { error: string }) {
  if (!error) return null;
  return (
    <div className="bg-red-50 border border-red-200 text-red-700 px-5 py-4 rounded-xl text-sm flex items-start gap-3 shadow-sm mb-6">
      <div className="flex-1">
        <h3 className="font-semibold text-red-800">Discovery error</h3>
        <p className="text-red-700 mt-1">{error}</p>
      </div>
    </div>
  );
}

function WizardContent(props: ExtractionWizardViewProps) {
  if (props.loading) return <WizardLoading message={props.loadingMessage} />;
  if (!props.question) return null;
  return <WizardSelectionForm {...props} question={props.question} />;
}

function WizardLoading({ message }: { message: string }) {
  return (
    <div className="flex flex-col items-center justify-center py-20 text-center animate-in fade-in duration-300">
      <div className="w-16 h-16 rounded-2xl bg-blue-100 flex items-center justify-center mb-6 shadow-inner relative overflow-hidden">
        <div className="absolute inset-0 bg-blue-500/20 animate-pulse" />
        <Loader2 className="w-8 h-8 text-blue-600 animate-spin relative z-10" />
      </div>
      <h3 className="text-lg font-semibold text-slate-900 mb-2">Finding operations</h3>
      <p className="text-sm text-slate-500 max-w-sm mx-auto leading-relaxed">{message}</p>
    </div>
  );
}

function WizardSelectionForm(props: ExtractionWizardViewProps & { question: QuestionData }) {
  const endpoints = props.question.endpoints || [];
  const onToggle = (id: string, selected: boolean) => toggleSelection(id, selected, props);
  return (
    <form onSubmit={props.onSubmit} className="flex flex-col h-full" toolname="submit_extraction_endpoints" tooldescription="Add the selected operations to this service definition.">
      <div className="flex-1 bg-white rounded-2xl border border-slate-200 shadow-sm overflow-hidden mb-6">
        <div className="p-5 border-b border-slate-100 bg-slate-50 flex items-center justify-between">
          <div className="flex items-center gap-3">
            <div className="w-8 h-8 rounded-full bg-blue-100 text-blue-600 flex items-center justify-center shrink-0"><Server className="w-4 h-4" /></div>
            <h2 className="text-sm font-semibold text-slate-900">Discovered operations</h2>
          </div>
          <button data-track="toggle_select_all_endpoints" type="button" onClick={() => toggleAllSelections(props, endpoints)} className="text-xs font-semibold text-blue-600 hover:text-blue-700 uppercase tracking-wider">
            {selectionLabel(props, endpoints.length)}
          </button>
        </div>
        <SelectionLimitNotice count={endpoints.length} maxSelections={props.maxSelections} />
        <div className="p-4 overflow-y-auto" style={{ maxHeight: "calc(100vh - 350px)" }}>
          <SelectionList {...props} endpoints={endpoints} onToggle={onToggle} />
        </div>
      </div>
      <SubmitSelectionButton {...props} />
    </form>
  );
}

function selectionLabel(props: ExtractionWizardViewProps, count: number) {
  if (props.selectedEndpoints.size > 0) return "Deselect All";
  if (count > props.maxSelections) return `Select First ${props.maxSelections}`;
  return "Select All";
}

function toggleAllSelections(props: ExtractionWizardViewProps, endpoints: IntegrationObject[]) {
  if (props.selectedEndpoints.size > 0) {
    props.setSelectedEndpoints(new Set());
    return;
  }
  const ids = endpoints.slice(0, props.maxSelections).map(endpoint => selectionID(endpoint, props.targetType));
  props.setSelectedEndpoints(new Set(ids));
}

function selectionID(endpoint: IntegrationObject, targetType: string) {
  if (targetType === "webhooks") return `${endpoint.method}|${endpoint.path}`;
  return `${endpoint.method}|${endpoint.path}|${endpoint.name || ""}`;
}

function toggleSelection(id: string, selected: boolean, props: ExtractionWizardViewProps) {
  const next = new Set(props.selectedEndpoints);
  if (selected && next.size >= props.maxSelections) {
    props.toast.error(`Select ${props.maxSelections} or fewer operations at a time.`);
    return;
  }
  if (selected) next.add(id); else next.delete(id);
  props.setSelectedEndpoints(next);
}

function SelectionLimitNotice({ count, maxSelections }: { count: number; maxSelections: number }) {
  if (count <= maxSelections) return null;
  return <div className="px-5 py-3 border-b border-amber-100 bg-amber-50 text-sm text-amber-800">Add up to {maxSelections} operations at a time.</div>;
}

function SelectionList(props: ExtractionWizardViewProps & { endpoints: IntegrationObject[]; onToggle: (id: string, selected: boolean) => void }) {
  if (props.targetType === "webhooks") {
    return <WebhookSelectionList webhooks={props.endpoints} selectedIds={props.selectedEndpoints} onToggle={props.onToggle} getId={webhookSelectionID} />;
  }
  return <EndpointSelectionList endpoints={props.endpoints} selectedIds={props.selectedEndpoints} onToggle={props.onToggle} getId={endpointSelectionID} />;
}

function webhookSelectionID(endpoint: SelectableEvent) {
  return `${endpoint.method}|${endpoint.path}`;
}

function endpointSelectionID(endpoint: IntegrationObject) {
  return `${endpoint.method}|${endpoint.path}|${endpoint.name || ""}`;
}

function SubmitSelectionButton(props: ExtractionWizardViewProps) {
  const disabled = props.submitting || props.selectedEndpoints.size === 0 || props.selectedEndpoints.size > props.maxSelections;
  return (
    <div className="flex justify-end gap-3 sticky bottom-0 bg-slate-50 pt-2 pb-6">
      <button data-track="extract_selected_endpoints" type="submit" disabled={disabled} className="w-full sm:w-auto px-8 py-3 bg-slate-950 hover:bg-slate-800 disabled:opacity-50 text-white font-medium rounded-xl transition-all flex items-center justify-center gap-2 shadow-sm cursor-pointer">
        <SubmitSelectionContent submitting={props.submitting} count={props.selectedEndpoints.size} />
      </button>
    </div>
  );
}

function SubmitSelectionContent({ submitting, count }: { submitting: boolean; count: number }) {
  if (submitting) return <><Loader2 className="w-5 h-5 animate-spin" /><span>Dispatching...</span></>;
  return <><PlayCircle className="w-5 h-5" /><span>Add {count} operations</span></>;
}
