import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import guidance from "./guidance.json" with { type: "json" };

/** Publishes optional detailed guidance without adding tools or expanding every model request. */
export function registerGuidance(server: McpServer): void {
  const guide = guidance.guide;
  // A fixed resource URI cannot reach provider data or arbitrary local files.
  server.registerResource(guide.name, guide.uri,
    { title: guide.title, description: guide.description, mimeType: "text/markdown" },
    async () => ({ contents: [{ uri: guide.uri, mimeType: "text/markdown", text: guide.text }] }));
  // Prompt selection is host/user controlled; its text stays identical to the readable resource.
  server.registerPrompt(guide.name, { title: guide.title, description: guide.description },
    async () => ({ description: guide.description, messages: [{ role: "user", content: { type: "text", text: guide.text } }] }));
}
