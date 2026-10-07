import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent as ReactKeyboardEvent, type UIEvent } from 'react';
import { ArrowUp, Check, ChevronRight, FileCode2, History, Loader2, MapPin, Plus, ScanLine, Sparkles, Square, Wrench, X } from 'lucide-react';
import { FusedAgentMarkdown } from './FusedAgentMarkdown';
import { FusedAgentHistory } from './FusedAgentHistory';
import type { FusedChatMessage } from './fused-chat-types';
import type { HarnestApproval } from '~/lib/fused-agent-transport';

const previewTasks = [
  { id: 'inspect', icon: ScanLine, title: 'Understand this page', description: 'Explain the options available here.', prompt: 'Explain the current page and what I can do here.' },
  { id: 'draft', icon: FileCode2, title: 'Help with a draft', description: 'Review a form or app before saving.', prompt: 'Review the visible draft and suggest what needs attention. Do not submit anything.' },
];
interface Props {
  popup: boolean; pageTitle: string; status: string; messages: FusedChatMessage[];
  composer: string; setComposer: (value: string) => void; includeContext: boolean;
  setIncludeContext: (value: boolean) => void; isSending: boolean; loadingHistory: boolean;
  error: string; sessionId: string | null; sendMessage: () => void; newConversation: () => void;
  closeAgent: () => void; stop: () => void; refreshAgentStatus: () => void;
  openSession: (id: string) => Promise<boolean>; deleteSession: (id: string) => Promise<void>;
  approval: HarnestApproval | null; decideApproval: (id: string, decision: 'approve' | 'deny') => void;
}
const focusStyle = 'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-violet-700';

/** Renders conversation activity, readable code examples, and explicit approval controls in each chat layout. */
export default function FusedAgentChat({ popup, pageTitle, status, messages, composer, setComposer, includeContext, setIncludeContext, isSending, loadingHistory, error, sessionId, sendMessage, newConversation, closeAgent, stop, refreshAgentStatus, openSession, deleteSession, approval, decideApproval }: Props) {
  const isReady = status === 'ready';
  const checkingAgentStatus = status === 'loading' || status === 'starting';
  const [historyOpen, setHistoryOpen] = useState(false);
  const panel = useRef<HTMLElement>(null);
  const input = useRef<HTMLTextAreaElement>(null);
  const scrollArea = useRef<HTMLDivElement>(null);
  const followResponse = useRef(true);

  // Opening chat focuses its composer without remounting it when a drawer changes the layout.
  useEffect(() => { input.current?.focus(); }, []);
  // Readers who scroll back keep their place instead of being pulled to every streamed token.
  useEffect(() => {
    if (followResponse.current) scrollArea.current?.scrollTo({ top: scrollArea.current.scrollHeight, behavior: 'instant' });
  }, [messages]);

  /** Escape closes only the focused chat, leaving the workspace drawer's own Escape handling independent. */
  function handlePanelKey(event: ReactKeyboardEvent<HTMLElement>) {
    // Events outside this panel belong to the workspace's active form or drawer.
    if (event.key === 'Escape' && !event.defaultPrevented) { event.preventDefault(); event.stopPropagation(); closeAgent(); }
  }
  /** Sends through Fused's guarded client-tool transport, never through a workspace form submit. */
  function send(event?: FormEvent) {
    event?.preventDefault();
    // History restoration and active responses must settle before starting another turn.
    if (isReady && !isSending && !loadingHistory) sendMessage();
    input.current?.focus();
  }
  /** Example prompts remain editable so the user chooses what page context to send. */
  function startExample(prompt: string) { setComposer(prompt); input.current?.focus(); }
  /** Starts a clean session while preserving the page's unsaved work. */
  function startNew() { newConversation(); setHistoryOpen(false); input.current?.focus(); }
  /** Opens actor-scoped history without discarding a currently streaming response. */
  function toggleHistory() { setHistoryOpen(previous => !previous); }
  /** A failed history fetch leaves the chooser visible for retry. */
  async function chooseSession(id: string) {
    // Only successful restoration may replace the history view with the conversation.
    if (await openSession(id)) setHistoryOpen(false);
  }
  /** Tracks user scroll intent so long replies remain readable. */
  function trackScroll(event: UIEvent<HTMLDivElement>) {
    const element = event.currentTarget;
    followResponse.current = element.scrollHeight - element.scrollTop - element.clientHeight < 80;
  }
  /** Enter sends, while IME composition and Shift+Enter retain native text entry. */
  function handleComposerKey(event: ReactKeyboardEvent<HTMLTextAreaElement>) {
    // Composition events must not send partially entered text.
    if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); send(); }
  }

  return (
    <>
      {/* Detail drawers get the desktop width; mobile chat always occupies the full viewport. */}
      <aside ref={panel} id="fused-assistant" role="complementary" aria-label="Fused assistant" aria-describedby="agent-preview-note" onKeyDown={handlePanelKey}
        className={`flex h-dvh w-full min-w-0 flex-col overflow-hidden border-slate-200 bg-slate-50 text-slate-900 ${popup ? 'md:fixed md:bottom-5 md:right-5 md:z-[60] md:h-[600px] md:max-h-[calc(100dvh-2.5rem)] md:w-[380px] md:rounded-2xl md:border md:shadow-2xl' : 'md:w-[360px] md:border-l xl:w-[420px]'}`}>

        <header className="flex shrink-0 items-center gap-3 border-b border-slate-200/80 bg-white px-5 py-4">
          <div className="flex h-9 w-9 items-center justify-center rounded-xl bg-violet-700 text-violet-100"><Sparkles className="h-[18px] w-[18px]" /></div>
          <div className="min-w-0 flex-1"><h2 id="agent-title" className="text-sm font-semibold tracking-tight">Fused</h2><p className="mt-0.5 text-[11px] text-slate-500">Your workspace assistant</p></div>
          <button type="button" onClick={toggleHistory} disabled={isSending || loadingHistory} aria-label="Conversations" aria-expanded={historyOpen} title="Conversations" className={`rounded-lg p-2 text-slate-500 hover:bg-slate-100 disabled:opacity-40 ${focusStyle}`}><History className="h-4 w-4" /></button>
          <button type="button" onClick={startNew} aria-label="New conversation" title="New conversation" className={`rounded-lg p-2 text-slate-500 hover:bg-slate-100 hover:text-slate-900 ${focusStyle}`}><Plus className="h-4 w-4" /></button>
          <button onClick={closeAgent} aria-label="Close Fused assistant" title="Close (Esc)" className={`rounded-lg p-2 text-slate-500 hover:bg-slate-100 hover:text-slate-900 ${focusStyle}`}><X className="h-4 w-4" /></button>
        </header>

        <div className="shrink-0 border-b border-slate-200/80 bg-white/60 px-5 py-3">
          <div className="flex items-center gap-2 text-xs text-slate-500"><MapPin className="h-3.5 w-3.5 shrink-0" /><span>Viewing</span><span className="truncate font-medium text-slate-800">{pageTitle}</span></div>
        </div>

        {!isReady && <div role="status" aria-live="polite" className="shrink-0 border-b border-slate-200 bg-slate-100/70 px-5 py-4">
          <p className="text-sm font-medium">{checkingAgentStatus ? 'Agent is starting' : 'Agent is temporarily unavailable'}</p>
          <p className="mt-1 text-xs leading-relaxed text-slate-500">{checkingAgentStatus ? 'Getting the agent ready. You can keep working while it starts.' : 'Your conversation and draft are still here. Check again after the agent connection is restored.'}</p>
          <button type="button" onClick={refreshAgentStatus} disabled={checkingAgentStatus} className={`mt-3 inline-flex items-center gap-2 rounded-lg border border-slate-300 bg-white px-3 py-1.5 text-xs font-medium disabled:opacity-50 ${focusStyle}`}>
            {checkingAgentStatus && <Loader2 className="h-3 w-3 animate-spin" />}{checkingAgentStatus ? 'Checking…' : 'Check connection'}
          </button>
        </div>}

        {/* Conversation history uses the same actor-scoped Harnest sessions as live replies. */}
        {historyOpen ? <FusedAgentHistory currentId={sessionId} disabled={loadingHistory} onSelect={chooseSession} onDelete={deleteSession} /> : <div ref={scrollArea} onScroll={trackScroll} className="min-h-0 flex-1 overflow-y-auto overscroll-contain scroll-smooth motion-reduce:scroll-auto">
          {messages.length === 0 ? (
            <div className="px-6 pb-7 pt-9 sm:pt-12">
              <div className="mb-6 flex h-12 w-12 items-center justify-center rounded-2xl border border-violet-900/10 bg-violet-50 text-violet-700"><Sparkles className="h-6 w-6" strokeWidth={1.5} /></div>
              <p className="mb-2 text-[10px] font-semibold uppercase tracking-[0.18em] text-slate-400">Your workspace companion</p>
              <h3 className="text-[26px] font-semibold leading-tight tracking-[-0.035em]">What are we<br />working on?</h3>
              <p className="mt-3 max-w-[300px] text-[13px] leading-[1.7] text-slate-500">Ask about your workspace, review a form, or improve your app’s code.</p>
              <div className="mt-7 space-y-2.5">
                {previewTasks.map(task => {
                  const Icon = task.icon;
                  return <button key={task.id} disabled={!isReady} onClick={() => startExample(task.prompt)} className={`group disabled:cursor-not-allowed disabled:opacity-50 flex w-full items-center gap-3 rounded-xl border border-slate-200 bg-white p-3.5 text-left transition hover:border-violet-800/30 hover:bg-violet-50/30 ${focusStyle}`}>
                    <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-slate-50 text-slate-600 group-hover:bg-violet-50 group-hover:text-violet-800"><Icon className="h-4 w-4" strokeWidth={1.6} /></div>
                    <div className="min-w-0 flex-1"><p className="text-xs font-semibold">{task.title}</p><p className="mt-1 text-[11px] text-slate-500">{task.description}</p></div><ChevronRight className="h-3.5 w-3.5 shrink-0 text-slate-400" />
                  </button>;
                })}
              </div>
              <p className="mt-5 text-[11px] leading-relaxed text-slate-400">Choose a task or describe what you need.</p>
            </div>
          ) : (
            // A consistent text cursor advertises selectable messages without switching at inline-code edges; links and buttons keep their pointer rules.
            <div role="log" aria-label="Agent conversation" aria-live="polite" aria-relevant="additions" style={{ cursor: 'text' }} className="min-w-0 max-w-full space-y-7 px-5 py-6">
              {messages.map(message => message.role === 'user' ? (
                <div key={message.id} className="ml-8 rounded-2xl rounded-br-sm border border-slate-200/70 bg-slate-100 px-4 py-3">

                  <FusedAgentMarkdown text={message.text} />
                </div>
              ) : (
                <div key={message.id} className="min-w-0 max-w-full">
                  <div className="mb-2.5 flex items-center gap-2 text-[11px] font-medium text-slate-500"><Sparkles className="h-3.5 w-3.5 text-violet-800" />Fused<span className="text-slate-300">/</span><span className="font-normal text-slate-400">Agent</span></div>
                  {message.tools?.length ? <div className="mb-3 space-y-1.5">{message.tools.map(tool => <div key={tool.id} className="flex items-center gap-2 rounded-lg border border-slate-200 bg-white px-2.5 py-2 text-[11px] text-slate-500">
                    {tool.status === 'running' ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : tool.status === 'completed' ? <Check className="h-3.5 w-3.5 text-violet-700" /> : <Wrench className="h-3.5 w-3.5" />}
                    <span className="min-w-0 flex-1 truncate">{tool.name.replaceAll('_', ' ')}</span><span>{tool.status}</span>
                  </div>)}</div> : null}
                  {message.text ? <FusedAgentMarkdown text={message.text} />
                    : isSending ? <p role="status" className="flex items-center gap-2 text-xs text-slate-400"><Loader2 className="h-3.5 w-3.5 animate-spin" />Working…</p> : null}
                </div>
              ))}
            </div>
          )}
        </div>}

        <footer className="shrink-0 border-t border-slate-200/80 bg-slate-50 px-4 pb-[max(1rem,env(safe-area-inset-bottom))] pt-3">
          {/* A suspended change stays reviewable; neither sending a message nor closing chat approves it. */}
          {approval && <section aria-label="Approval required" role="region" className="mb-3 rounded-xl border border-violet-200 bg-white p-3 text-sm">
            <p className="font-semibold text-slate-900">Approval required</p>
            <p className="mt-2 max-h-36 overflow-auto whitespace-pre-wrap break-words text-xs leading-relaxed text-slate-600">{approval.message}</p>
            <div className="mt-3 flex justify-end gap-2">
              <button type="button" onClick={() => decideApproval(approval.id, 'deny')} className={`rounded-lg border border-slate-200 px-3 py-1.5 text-xs font-medium text-slate-600 ${focusStyle}`}>Deny</button>
              <button type="button" onClick={() => decideApproval(approval.id, 'approve')} className={`rounded-lg bg-violet-700 px-3 py-1.5 text-xs font-medium text-white ${focusStyle}`}>Approve</button>
            </div>
          </section>}
          {error && <p role="alert" className="mb-3 rounded-lg bg-red-50 px-3 py-2 text-xs leading-relaxed text-red-700">{error}</p>}
          <form onSubmit={send} className="rounded-2xl border border-slate-300/80 bg-white p-3 shadow-sm focus-within:border-violet-800/50 focus-within:ring-2 focus-within:ring-violet-800/5">
            <button type="button" aria-pressed={includeContext} onClick={() => setIncludeContext(!includeContext)} aria-label="Include current page" title="Use visible page content. Sensitive values stay hidden." className={`mb-2 flex max-w-full items-center gap-1.5 rounded-md px-2 py-1 text-[10px] ${includeContext ? 'bg-violet-50 text-violet-900' : 'bg-slate-100 text-slate-500'} ${focusStyle}`}>
              <MapPin className="h-3 w-3 shrink-0" /><span className="truncate">{includeContext ? pageTitle : 'Page context off'}</span>{includeContext && <Check className="h-3 w-3 shrink-0" />}
            </button>
            <label htmlFor="agent-message" className="sr-only">Message Fused</label>
            <textarea ref={input} id="agent-message" value={composer} onChange={event => setComposer(event.target.value)} maxLength={16384} rows={2}
              onKeyDown={handleComposerKey}
              placeholder="Describe what you’d like to do…"
              className="block max-h-36 min-h-12 w-full resize-none bg-transparent text-[13px] leading-relaxed text-slate-800 outline-none placeholder:text-slate-400" />
            <div className="mt-2 flex items-center justify-between gap-2"><span className="text-[10px] text-slate-400">Enter to send · Shift + Enter for a new line</span>{isSending ? <button type="button" onClick={stop} aria-label="Stop response" className={`flex h-8 w-8 items-center justify-center rounded-lg bg-slate-800 text-white ${focusStyle}`}><Square className="h-3.5 w-3.5" /></button> : <button type="submit" disabled={!isReady || !composer.trim() || loadingHistory} aria-label="Send message" className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-violet-700 text-white transition hover:bg-violet-800 disabled:cursor-not-allowed disabled:bg-slate-100 disabled:text-slate-300 ${focusStyle}`}><ArrowUp className="h-4 w-4" /></button>}</div>
          </form>
          <p id="agent-preview-note" className="mt-2.5 text-center text-[10px] leading-relaxed text-slate-400">Review changes before saving.</p>
        </footer>
      </aside>
    </>
  );
}
