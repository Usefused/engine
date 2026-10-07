import { createContext, useContext } from "react";
import type { AppServicePin } from "~/lib/app-describe-contract";

export interface FusedEditorBridge {
  fieldID: string;
  view: "typescript" | "yaml";
  configError: string;
  setView: (view: "typescript" | "yaml") => void;
  services: Record<string, AppServicePin>;
  compile: () => Promise<unknown>;
  revise: (goal: string, signal: AbortSignal) => Promise<unknown>;
  available: boolean;
}
export interface FusedDraftBuilderBridge {
  available: boolean;
  describe: (goal: string, signal: AbortSignal) => Promise<unknown>;
}
export interface FusedAgentContextValue {
  isOpen: boolean;
  open: (message?: string) => void;
  registerEditor: (bridge: FusedEditorBridge) => () => void;
  registerDraftBuilder: (bridge: FusedDraftBuilderBridge) => () => void;
}
export const FusedAgentContext = createContext<FusedAgentContextValue | null>(null);
/** Connects existing page editors to the workspace-wide conversation without giving the agent a second source store. */
export function useFusedAgent() { return useContext(FusedAgentContext); }
