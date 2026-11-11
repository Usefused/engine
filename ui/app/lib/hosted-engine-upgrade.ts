export type HostedEngineState = {
  available: boolean;
  hosted: boolean;
  current_image?: string;
  target_image?: string;
  desired_generation?: number;
  observed_generation?: number;
  status?: string;
  message?: string;
  update_available: boolean;
};

export type EngineUpgradeIntent = { id: string; expected_generation: number; target_image: string };

// upgradeCompleted requires observed readiness for the exact approved release, even after a lost POST response.
export function upgradeCompleted(state: HostedEngineState, intent: EngineUpgradeIntent): boolean {
  return state.status === "ready" && state.desired_generation === state.observed_generation &&
    (state.observed_generation ?? 0) > intent.expected_generation && state.current_image === intent.target_image;
}

// upgradeInProgress separates temporary reconnects from failed or completed rollouts.
export function upgradeInProgress(state: HostedEngineState | null): boolean {
  return Boolean(state?.hosted && ["pending", "provisioning", "upgrading", "resuming"].includes(state.status ?? ""));
}

// readUpgradeIntent validates non-secret session metadata before it can be used for an explicit retry.
export function readUpgradeIntent(value: string | null): EngineUpgradeIntent | null {
  try {
    const intent = JSON.parse(value ?? "null");
    // Corrupt or stale browser storage must never supply an arbitrary update request.
    if (!validUpgradeIntent(intent)) return null;
    return { id: intent.id, expected_generation: intent.expected_generation, target_image: intent.target_image };
  } catch {
    // An unreadable saved intent is discarded without submitting a mutation.
    return null;
  }
}

// validUpgradeIntent bounds persisted metadata before an explicit retry can use it.
function validUpgradeIntent(intent: EngineUpgradeIntent | null): intent is EngineUpgradeIntent {
  return Boolean(intent && typeof intent.id === "string" && /^[0-9a-f-]{36}$/i.test(intent.id) &&
    Number.isSafeInteger(intent.expected_generation) && intent.expected_generation >= 1 &&
    typeof intent.target_image === "string" && intent.target_image.length > 0 && intent.target_image.length <= 512);
}

// upgradeStatusMessage distinguishes a confirmed rollout from an uncertain submission or a reported failure.
export function upgradeStatusMessage(state: HostedEngineState, pending: EngineUpgradeIntent | null): string {
  // A reported failure requires an explicit retry or operator intervention.
  if (state.status === "failed") return state.message || "The update failed. Retry the saved update or contact your operator.";
  if (upgradeInProgress(state)) return "Updating Engine. This page reconnects automatically after the restart.";
  if (pending) return "This update has not been confirmed. Retry the saved request, or clear it if another update superseded it.";
  return "";
}
