export interface AppActivityIssue {
  message: string;
  tone: "neutral" | "error";
}

export function appActivityIssue(cause: unknown, transport: "sdk" | "mcp"): AppActivityIssue {
  const rawMessage = cause instanceof Error ? cause.message : "";
  if (rawMessage.toLowerCase().includes("app not found")) {
    const appName = transport === "mcp" ? "MCP server" : "app";
    return {
      message: `This ${appName} is not active on this Engine, so local execution activity is unavailable.`,
      tone: "neutral",
    };
  }
  return {
    message: "Execution activity is temporarily unavailable. Try again shortly.",
    tone: "error",
  };
}
