export interface ArtifactActivityIssue {
  message: string;
  tone: "neutral" | "error";
}

export function artifactActivityIssue(cause: unknown, transport: "sdk" | "mcp"): ArtifactActivityIssue {
  const rawMessage = cause instanceof Error ? cause.message : "";
  if (rawMessage.toLowerCase().includes("sdk scope not found")) {
    const artifactName = transport === "mcp" ? "MCP server" : "app";
    return {
      message: `This ${artifactName} is not active on this Engine, so local execution activity is unavailable.`,
      tone: "neutral",
    };
  }
  return {
    message: "Execution activity is temporarily unavailable. Try again shortly.",
    tone: "error",
  };
}
