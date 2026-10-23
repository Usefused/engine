import { expect, it } from "vitest";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import guidance from "./guidance.json" with { type: "json" };
import { registerGuidance } from "./guidance.js";

// Real SDK negotiation must advertise and return identical optional guidance on both native surfaces.
it("serves the same workflow guide through resource reads and prompt selection", async () => {
  const server = new McpServer({ name: "guidance-test", version: "1" });
  registerGuidance(server);
  const client = new Client({ name: "guidance-test", version: "1" });
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  await server.connect(serverTransport);
  await client.connect(clientTransport);
  try {
    expect(client.getServerCapabilities()).toMatchObject({ resources: {}, prompts: {} });
    expect(await client.listResources()).toMatchObject({ resources: [{ uri: guidance.guide.uri, name: guidance.guide.name, mimeType: "text/markdown" }] });
    expect(await client.listPrompts()).toMatchObject({ prompts: [{ name: guidance.guide.name }] });
    const resource = await client.readResource({ uri: guidance.guide.uri });
    const prompt = await client.getPrompt({ name: guidance.guide.name });
    expect(resource.contents[0]).toMatchObject({ text: guidance.guide.text });
    expect(prompt.messages).toEqual([{ role: "user", content: { type: "text", text: guidance.guide.text } }]);
    // URI lookalikes must not become paths, provider reads, or another guide namespace.
    await expect(client.readResource({ uri: guidance.guide.uri + "/../private" })).rejects.toThrow();
    await expect(client.getPrompt({ name: "missing" })).rejects.toThrow();
  } finally {
    await client.close();
    await server.close();
  }
});
