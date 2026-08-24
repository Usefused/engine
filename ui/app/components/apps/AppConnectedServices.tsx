import { ChevronDown, ExternalLink } from "lucide-react";
import { Link } from "@remix-run/react";
import { EndpointRow, WebhookRow } from "~/components/EndpointRow";
import { serviceDetailPath } from "~/lib/service-navigation";
import { appSelectionDisplayRows } from "~/lib/app-selection-v3";
import type { AppConnectedServiceSelection } from "~/lib/app-connected-services";

export type { AppConnectedServiceSelection } from "~/lib/app-connected-services";

/** Describes the immutable selection without implying that IDs were relabelled later. */
function selectionSummary(selection: AppConnectedServiceSelection): string {
  if (selection.select_all) return "All operations";
  const operationCount = selection.operation_names?.length || selection.endpoint_ids.length;
  const webhookCount = selection.webhook_names?.length || selection.webhook_ids.length;
  const labels = [countLabel(operationCount, "operation"), countLabel(webhookCount, "webhook")].filter(Boolean);
  return labels.join(" · ") || "No selected resources";
}

/** Keeps singular and plural resource labels consistent in every app detail page. */
function countLabel(count: number, label: string): string {
  if (count === 0) return "";
  return `${count} ${label}${count === 1 ? "" : "s"}`;
}

function AllOperationsNotice({ selection }: { selection: AppConnectedServiceSelection }) {
  if (!selection.select_all) return null;
  const count = selection.endpoint_count;
  return (
    <div className="border-b border-slate-100 bg-blue-50/40 px-4 py-3 text-sm text-slate-600 sm:px-5">
      All operations in this pinned service version are enabled{count ? ` (${count.toLocaleString()} currently available)` : ""}.
    </div>
  );
}

function SelectedOperations({ selection }: { selection: AppConnectedServiceSelection }) {
  if (selection.select_all) return <AllOperationsNotice selection={selection} />;
  const operations = appSelectionDisplayRows(selection.endpoint_ids, selection.operation_names);
  if (operations.length === 0) return null;
  return (
    <section aria-label="Enabled operations">
      <div className="border-b border-slate-100 bg-slate-50 px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-slate-600 sm:px-5">
        Operations
      </div>
      <div className="divide-y divide-slate-100">
        {operations.map((operation) => <EndpointRow key={operation.id} ep={operation} />)}
      </div>
    </section>
  );
}

function SelectedWebhooks({ selection }: { selection: AppConnectedServiceSelection }) {
  const webhooks = appSelectionDisplayRows(selection.webhook_ids, selection.webhook_names);
  if (webhooks.length === 0 && !selection.webhook_select_all) return null;
  return (
    <section aria-label="Enabled webhooks">
      <div className="border-y border-slate-100 bg-slate-50 px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-slate-600 sm:px-5">
        Webhooks
      </div>
      {selection.webhook_select_all ? (
        <div className="px-4 py-3 text-sm text-slate-600 sm:px-5">All webhooks in this pinned service version are enabled.</div>
      ) : (
        <div className="divide-y divide-slate-100">
          {webhooks.map((webhook) => <WebhookRow key={webhook.id} wh={webhook} />)}
        </div>
      )}
    </section>
  );
}

function ServiceLink({ selection }: { selection: AppConnectedServiceSelection }) {
  if (!selection.service_slug) return null;
  return (
    <Link
      to={serviceDetailPath(selection.service_id, selection.service_slug)}
      className="inline-flex items-center gap-1 text-xs font-medium text-blue-600 hover:text-blue-700"
    >
      View service <ExternalLink className="h-3 w-3" />
    </Link>
  );
}

/** Renders service and operation identity persisted with an immutable app version. */
export function AppConnectedServices({ selections }: { selections: AppConnectedServiceSelection[] }) {
  if (selections.length === 0) {
    return <p className="rounded-lg border border-slate-200 bg-white px-4 py-8 text-center text-sm text-slate-500">No services connected.</p>;
  }

  return (
    <div className="overflow-hidden rounded-xl border border-slate-200 bg-white divide-y divide-slate-100">
      {selections.map((selection) => (
        <details key={selection.service_id} className="group">
          <summary className="flex cursor-pointer list-none items-start justify-between gap-4 bg-slate-50 px-4 py-3 marker:content-none hover:bg-slate-100 sm:items-center sm:px-5">
            <div className="min-w-0">
              <div className="break-words text-sm font-semibold text-slate-800">{selection.service_name || "Unnamed service"}</div>
              {selection.service_version_name ? <div className="mt-0.5 break-all text-xs text-slate-500">{selection.service_version_name}</div> : null}
            </div>
            <div className="flex shrink-0 items-center gap-2 text-right text-xs text-slate-500">
              <span>{selectionSummary(selection)}</span>
              <ChevronDown className="h-4 w-4 transition-transform group-open:rotate-180" />
            </div>
          </summary>
          <div className="bg-white">
            <SelectedOperations selection={selection} />
            <SelectedWebhooks selection={selection} />
            {selection.service_slug ? <div className="border-t border-slate-100 px-4 py-3 sm:px-5"><ServiceLink selection={selection} /></div> : null}
          </div>
        </details>
      ))}
    </div>
  );
}
