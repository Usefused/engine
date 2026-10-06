export interface FusedChatMessage {
  id: string;
  role: 'user' | 'assistant';
  text: string;
  tools?: { id: string; name: string; status: 'running' | 'completed' | 'failed' | 'stopped' }[];
}
