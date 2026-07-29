import { useEffect, useState } from "react";
import { fetchEventSource } from "@microsoft/fetch-event-source";
import { api, BASE, type IntegrationObject } from "~/lib/api";
import { getApiKey } from "~/lib/session";

interface ImportStreamEvent {
  type: "connected" | "thinking" | "extracted" | "complete" | "error";
  message?: string;
  payload?: IntegrationObject;
  integration_id?: string;
  version?: string;
}

interface ImportProgress {
  active: boolean;
  status: string;
  error?: string;
}

interface ImportEventActions {
  controller: AbortController;
  setProgress: (progress: ImportProgress) => void;
  onExtractedPayload: (payload: IntegrationObject) => void;
  onComplete: () => void;
  onClearSession: () => void;
}

function progress(active: boolean, status: string, error?: string): ImportProgress {
  return { active, status, error };
}

async function completeImport(event: ImportStreamEvent, actions: ImportEventActions) {
  actions.setProgress(progress(false, event.message || "Extraction complete."));
  try {
    if (event.integration_id) {
      await api.workspace.addService(event.integration_id, "", event.version || "");
    }
  } catch (error) {
    const message = error instanceof Error ? error.message : "Workspace auto-registration failed.";
    actions.setProgress(progress(false, "Extraction complete.", message));
    actions.onComplete();
    actions.controller.abort();
    return;
  }
  actions.onComplete();
  actions.onClearSession();
  actions.controller.abort();
}

async function handleImportEvent(event: ImportStreamEvent, actions: ImportEventActions) {
  switch (event.type) {
    case "connected":
      actions.setProgress(progress(true, "Extraction is running..."));
      return;
    case "thinking":
      actions.setProgress(progress(true, event.message || "Extracting selected endpoints..."));
      return;
    case "extracted":
      actions.setProgress(progress(true, event.message || "Extracted endpoint..."));
      if (event.payload) actions.onExtractedPayload(event.payload);
      return;
    case "complete":
      await completeImport(event, actions);
      return;
    case "error":
      actions.setProgress(progress(false, "Extraction failed.", event.message || "The extraction job failed."));
      actions.controller.abort();
  }
}

export function useImportSession(
  importSessionId: string | null,
  onExtractedPayload: (payload: IntegrationObject) => void,
  onComplete: () => void,
  onClearSession: () => void
) {
  const [importProgress, setImportProgress] = useState<ImportProgress | null>(null);

  useEffect(() => {
    if (!importSessionId) {
      setImportProgress(null);
      return;
    }

    const controller = new AbortController();
    const actions: ImportEventActions = {
      controller,
      setProgress: setImportProgress,
      onExtractedPayload,
      onComplete,
      onClearSession,
    };
    setImportProgress(progress(true, "Connecting to extraction stream..."));
    let retryDelay = 2000;
    const maxRetryDelay = 60000;

    fetchEventSource(`${BASE}/integrations/session/${importSessionId}/stream`, {
      headers: { "X-API-Key": getApiKey() ?? "" },
      signal: controller.signal,
      async onopen(response) {
        if (!response.ok) throw new Error(`Failed to connect to extraction stream: ${response.status}`);
        retryDelay = 2000;
      },
      async onmessage(message) {
        try {
          await handleImportEvent(JSON.parse(message.data) as ImportStreamEvent, actions);
        } catch (error) {
          console.error("Failed to parse extraction stream event", error);
        }
      },
      onerror(error) {
        if (controller.signal.aborted) throw error;
        setImportProgress(progress(true, `Extraction stream disconnected. Reconnecting in ${retryDelay}ms...`));
        const delay = retryDelay;
        retryDelay = Math.min(retryDelay * 2, maxRetryDelay);
        return delay;
      },
    });

    return () => controller.abort();
  }, [importSessionId]);

  return { importProgress };
}
