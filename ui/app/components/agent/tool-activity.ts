import type { FusedChatMessage as AgentMessage } from './fused-chat-types';

type ToolActivity = NonNullable<AgentMessage['tools']>[number];

/** Reconciles streamed and suspended tool IDs without duplicating a single visible action. */
export function updateToolActivity(
  tools: ToolActivity[] = [],
  activity: ToolActivity,
  claimStreamedCall = false,
): ToolActivity[] {
  let index = tools.findIndex(tool => tool.id === activity.id);
  // Harnest's client request ID differs from its streamed model tool ID.
  // Adopt that pending entry once; subsequent continuations keep unique IDs.
  if (index < 0 && claimStreamedCall) {
    index = tools.findIndex(tool => tool.status === 'running' && tool.name === activity.name);
  }
  // New calls append; a resumed call updates its existing status card.
  return index < 0 ? [...tools, activity] : tools.map((tool, at) => at === index ? activity : tool);
}
