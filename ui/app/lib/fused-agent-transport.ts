import { getCSRFToken } from './session.ts';
import { credentialedRequestInit } from './browser-request.ts';

export interface HarnestSession {
  id: string;
  userId: string;
  state: Record<string, unknown>;
  applicationData: Record<string, unknown>;
  createdAt: string | null;
  updatedAt: string | null;
  metadata: Record<string, unknown>;
}

export interface HarnestSessionMessage {
  id: string;
  role: string;
  content: unknown;
  createdAt: string | null;
  metadata: Record<string, unknown>;
}

export interface HarnestStreamEvent {
  type: string;
  sequence?: number;
  responseId?: string;
  sessionId?: string;
  delta?: string;
  id?: string;
  callId?: string;
  name?: string;
  arguments?: Record<string, unknown>;
  output?: unknown;
  outputText?: string;
  status?: string;
  error?: string;
  clientTool?: HarnestClientTool;
  approval?: HarnestApproval;
  requiredAction?: { type: string } & Partial<HarnestClientTool & HarnestApproval>;
}

export interface HarnestApproval {
  id: string;
  callId: string;
  action: string;
  message: string;
  expiresAt: string;
}

export interface HarnestClientTool {
  id: string;
  callId: string;
  name: string;
  arguments: Record<string, unknown>;
}

type SessionPage = { sessions: HarnestSession[]; nextCursor: string | null };
type MessagePage = {
  sessionId: string;
  userId: string;
  messages: HarnestSessionMessage[];
  nextCursor: string | null;
};

/** Uses the normal browser session and CSRF protection; model credentials never enter this transport. */
async function request(path: string, init: RequestInit = {}) {
  const response = await fetch(`/agent/${path}`, credentialedRequestInit(init, getCSRFToken()));
  // A failed Engine connection must not be interpreted as an empty assistant response.
  if (!response.ok) throw new Error(`Fused agent request failed (${response.status}). Check agent availability or sign in again.`);
  return response;
}

export const harnest = {
  /** Lists only sessions owned by the authenticated actor. */
  async listSessions(signal?: AbortSignal): Promise<HarnestSession[]> {
    const response = await request('sessions?limit=100', { signal });
    return ((await response.json()) as SessionPage).sessions || [];
  },

  /** Starts an actor-owned conversation whose history can survive page navigation. */
  async createSession(title: string, signal?: AbortSignal): Promise<HarnestSession> {
    const response = await request('sessions', {
      method: 'POST',
      body: JSON.stringify({ state: { title, source: 'fused-web' } }),
      signal,
    });
    return response.json();
  },

  /** Loads the existing conversation without copying state into page fields. */
  async getMessages(sessionId: string, signal?: AbortSignal): Promise<HarnestSessionMessage[]> {
    const response = await request(`sessions/${encodeURIComponent(sessionId)}/messages?limit=100`, { signal });
    return ((await response.json()) as MessagePage).messages || [];
  },

  /** Deletes only the selected conversation through its authenticated transport. */
  async deleteSession(sessionId: string): Promise<void> {
    await request(`sessions/${encodeURIComponent(sessionId)}`, { method: 'DELETE' });
  },

  /** Consumes SSE and resumes the exact suspended client action, never replaying an action implicitly. */
  async streamResponse(
    input: string,
    sessionId: string,
    onEvent: (event: HarnestStreamEvent) => void,
    signal?: AbortSignal,
    executeClientTool?: (tool: HarnestClientTool) => Promise<unknown>,
    pageContext?: unknown,
    requestApproval?: (approval: HarnestApproval) => Promise<'approve' | 'deny'>,
  ): Promise<void> {
    const response = await request('responses', {
      method: 'POST',
      body: JSON.stringify({
        input,
        sessionId,
        stream: true,
        metadata: { source: 'fused-web', pageContext },
      }),
      signal,
      headers: { Accept: 'text/event-stream' },
    });

    if (!response.body) throw new Error('The Fused agent returned an empty stream.');

    let pending: HarnestClientTool | undefined;
    let approval: HarnestApproval | undefined;
    let completed = false;
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';

    /** Decodes bounded protocol frames and records authoritative completion or tool suspension. */
    const consumeFrame = (frame: string) => {
      const data = frame
        .split(/\r?\n/)
        .filter((line) => line.startsWith('data:'))
        .map((line) => line.slice(5).trimStart())
        .join('\n');
      if (!data) return;
      const event = JSON.parse(data) as HarnestStreamEvent;
      if (event.type === 'client_tool.requested') pending = event.clientTool;
      // Approval is a separate runtime suspension, before the frontend action is issued.
      if (event.type === 'approval.requested') approval = event.approval;
      if (event.type === 'response.completed') {
        completed = true;
        if (event.status === 'requires_action' && !['client_tool', 'human_approval'].includes(event.requiredAction?.type ?? '')) {
          throw new Error('The agent requested an unsupported action. Start a new conversation.');
        }
        if (event.status === 'requires_action') {
          const action = event.requiredAction;
          // Approval payloads contain a public description, never executable frontend arguments.
          if (action?.type === 'human_approval') approval = action as HarnestApproval;
          else {
            // Incomplete client actions must not reach the page executor.
            if (!action?.id || !action.name || !action.arguments) throw new Error('The agent returned an incomplete frontend action.');
            pending = action as HarnestClientTool;
          }
        } else if (event.status !== 'completed') {
          throw new Error('The agent did not complete the response.');
        }
      }
      onEvent(event);
    };

    try { while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      // An unterminated server frame cannot grow browser memory without bound.
      if (buffer.length > 1024 * 1024) throw new Error('Agent response frame exceeds the limit.');
      const frames = buffer.split(/\r?\n\r?\n/);
      buffer = frames.pop() || '';
      frames.forEach(consumeFrame);
    }

    buffer += decoder.decode();
    if (buffer.trim()) consumeFrame(buffer);
    } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
    if (!completed) throw new Error('The agent connection ended before the response completed.');

    // Harnest closes SSE at a client-tool boundary. Submit the result to resume
    // that exact suspended invocation; JSON continuations may request more tools.
    const handled = new Set<string>();
    for (let count = 0; pending || approval; count++) {
      signal?.throwIfAborted();
      const actionId = approval?.id ?? pending?.id;
      // Repeated or unbounded suspensions cannot replay an accepted change.
      if (!actionId || count >= 48 || handled.has(actionId)) throw new Error('The agent exceeded the action limit.');
      handled.add(actionId);
      let resumed: Response;
      // No runtime approval is submitted until the user explicitly chooses a decision.
      if (approval) {
        if (!requestApproval || !approval.message || !approval.expiresAt) throw new Error('Approval is unavailable. No change was made.');
        const decision = await requestApproval(approval);
        signal?.throwIfAborted();
        resumed = await request(`approvals/${encodeURIComponent(approval.id)}`, { method: 'POST', body: JSON.stringify({ decision }), signal });
        approval = undefined;
      } else {
        // Only concrete frontend requests may invoke the bounded page executor.
        if (!executeClientTool || !pending?.name) throw new Error('The agent requested an unavailable frontend tool.');
        const output = await executeClientTool(pending);
        signal?.throwIfAborted();
        resumed = await request(`client-tools/${encodeURIComponent(pending.id)}`, { method: 'POST', body: JSON.stringify({ output }), signal });
        pending = undefined;
      }
      const result = await resumed.json() as HarnestStreamEvent;
      if (result.status === 'requires_action') {
        const action = result.requiredAction;
        // Each protected call receives its own approval, including chained continuations.
        if (action?.type === 'human_approval') {
          approval = action as HarnestApproval;
          onEvent({ ...result, type: 'approval.requested', approval });
        } else {
          // Unknown runtime actions fail closed rather than being treated as reads.
          if (action?.type !== 'client_tool' || !action.id || !action.name || !action.arguments) throw new Error('The agent requested an unsupported action.');
          pending = action as HarnestClientTool;
          onEvent({ ...result, type: 'client_tool.requested', clientTool: pending });
        }
      } else {
        if (result.status !== 'completed') throw new Error('The agent did not complete the response.');
        pending = undefined;
        onEvent({ ...result, type: 'response.completed' });
      }
    }
  },
};

/** Gives untitled sessions a stable label without including page contents. */
export function sessionTitle(session: HarnestSession): string {
  const title = session.state?.title;
  return typeof title === 'string' && title.trim()
    ? title
    : `Conversation ${session.id.slice(0, 8)}`;
}

/** Extracts conversational text while keeping model content out of HTML rendering. */
export function messageText(content: unknown): string {
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return content == null ? '' : JSON.stringify(content);
  return content
    .map((item) => {
      if (typeof item === 'string') return item;
      if (item && typeof item === 'object' && 'text' in item) {
        return String((item as { text: unknown }).text || '');
      }
      return '';
    })
    .join('');
}
