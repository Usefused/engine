export interface AppActivityIssue {
  message: string;
  tone: "neutral" | "error";
}

/** Explains missing local activity in workspace terms without leaking storage errors. */
export function appActivityIssue(cause: unknown, transport: "sdk" | "mcp"): AppActivityIssue {
  const rawMessage = cause instanceof Error ? cause.message : "";
  // A missing local app is a neutral availability state rather than a failed execution.
  if (rawMessage.toLowerCase().includes("app not found")) {
    const appName = transport === "mcp" ? "MCP server" : "app";
    return {
      message: `This ${appName} is not active in this Fused workspace, so local execution activity is unavailable.`,
      tone: "neutral",
    };
  }
  return {
    message: "Execution activity is temporarily unavailable. Try again shortly.",
    tone: "error",
  };
}
