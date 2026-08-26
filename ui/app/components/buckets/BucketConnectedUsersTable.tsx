import { useState } from "react";
import {
  ChevronDown,
  ChevronUp,
  Loader2,
  MapPin,
	RefreshCw,
  Trash2,
  UserRound,
} from "lucide-react";
import {
  type AuthConnection,
  type BucketServiceSummary,
  type ActivatedService,
  type ConnectionResource,
  api,
} from "~/lib/api";
import { BucketPagination } from "~/components/buckets/BucketPagination";
import { BucketServiceSelect } from "~/components/buckets/BucketServiceSelect";
import { useToast } from "~/components/Toast";
import { AuthNameField } from "~/components/AuthNameField";

type BucketConnectedUsersTableProps = {
  loading: boolean;
  connections: AuthConnection[];
  services: BucketServiceSummary[];
  workspaceServices: ActivatedService[];
  total: number;
  page: number;
  pageSize: number;
  serviceSearch: string;
  serviceFilter: string;
  deletingConnectionId: string | null;
  onPageChange: (page: number) => void;
  onServiceSearchChange: (search: string) => void;
  onServiceFilterChange: (serviceId: string) => void;
  onRemoveConnection: (connection: AuthConnection) => void;
  canManage: boolean;
};

/** Renders connected users and gates all connection mutations. */
export function BucketConnectedUsersTable({
  loading,
  connections,
  services,
  workspaceServices,
  total,
  page,
  pageSize,
  serviceSearch,
  serviceFilter,
  deletingConnectionId,
  onPageChange,
  onServiceSearchChange,
  onServiceFilterChange,
  onRemoveConnection,
  canManage,
}: BucketConnectedUsersTableProps) {
  return (
    <div>
      <ConnectedUserToolbar
        services={services}
        serviceSearch={serviceSearch}
        serviceFilter={serviceFilter}
        onServiceSearchChange={onServiceSearchChange}
        onServiceFilterChange={onServiceFilterChange}
      />
      {loading ? (
        <div className="px-5 py-10 text-center text-sm text-slate-400">
          Loading connected users...
        </div>
      ) : connections.length === 0 ? (
        <div className="px-5 py-10 text-center text-sm text-slate-400">
          No connected users in this credential set.
        </div>
      ) : (
        <div className="flex flex-col gap-3 p-5">
          {connections.map((connection) => (
            <ConnectedUserRow
              key={connection.id}
              connection={connection}
              serviceName={serviceName(
                workspaceServices,
                connection.service_id
              )}
              deleting={deletingConnectionId === connection.id}
              onRemoveConnection={onRemoveConnection}
              canManage={canManage}
            />
          ))}
        </div>
      )}
      <BucketPagination
        total={total}
        page={page}
        pageSize={pageSize}
        onPageChange={onPageChange}
      />
    </div>
  );
}

// ConnectedUserToolbar explains that the bucket stores a service-scheme binding, not a new scheme definition.
function ConnectedUserToolbar({
  services,
  serviceSearch,
  serviceFilter,
  onServiceSearchChange,
  onServiceFilterChange,
}: {
  services: BucketServiceSummary[];
  serviceSearch: string;
  serviceFilter: string;
  onServiceSearchChange: (search: string) => void;
  onServiceFilterChange: (serviceId: string) => void;
}) {
  return (
    <div className="border-b border-slate-100 px-5 py-3">
      <BucketServiceSelect
        id="connected-user-service-filter"
        label="Service"
        placeholder="All services"
        options={bucketServiceOptions(services)}
        search={serviceSearch}
        selectedServiceId={serviceFilter}
        className="max-w-xs"
        allowAll
        allLabel="All services"
        onSearchChange={onServiceSearchChange}
        onSelectedServiceChange={onServiceFilterChange}
      />
      <p className="mt-2 text-xs leading-relaxed text-slate-500">Stored auth name links each connection to a service-defined authentication scheme. Use it as <code>auth.name</code> in SDK/MCP config; <code>end_user_ref</code> identifies the connected user.</p>
    </div>
  );
}

/** Shows the connection's stored scheme name independently of resource expansion and gates mutations. */
function ConnectedUserRow({
  connection,
  serviceName,
  deleting,
  onRemoveConnection,
  canManage,
}: {
  connection: AuthConnection;
  serviceName: string;
  deleting: boolean;
  onRemoveConnection: (connection: AuthConnection) => void;
  canManage: boolean;
}) {
  const [expanded, setExpanded] = useState(false);
  const [resources, setResources] = useState<ConnectionResource[]>([]);
  const [loadingResources, setLoadingResources] = useState(false);
	const [rediscovering, setRediscovering] = useState(false);
  const toast = useToast();

  // Resource metadata is loaded only when the row is opened, keeping the
  // paginated connection list to one GraphQL request instead of one per user.
  const toggleExpanded = async () => {
    const opening = !expanded;
    setExpanded(opening);
    if (!opening || resources.length > 0) return;
    setLoadingResources(true);
    try {
      setResources(await api.workspace.listConnectionResources(connection.id));
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : "Failed to load resources"
      );
    } finally {
      setLoadingResources(false);
    }
  };

  // Refreshing from Engine after the mutation makes the single-default
  // database rule authoritative instead of reproducing it in component state.
  const setDefaultResource = async (resourceId: string) => {
    try {
      await api.workspace.setDefaultConnectionResource(
        connection.id,
        resourceId
      );
      setResources(await api.workspace.listConnectionResources(connection.id));
      toast.success("Default resource updated");
    } catch (error) {
      toast.error(
        error instanceof Error
          ? error.message
          : "Failed to update default resource"
      );
    }
  };

	// rediscoverResources delegates token refresh and authoritative reconciliation
	// to Engine so no provider response or credential enters browser state.
	const rediscoverResources = async () => {
		setRediscovering(true);
		try {
			setResources(
				await api.workspace.rediscoverConnectionResources(connection.id)
			);
			toast.success("Connection resources refreshed");
		} catch (error) {
			toast.error(
				error instanceof Error ? error.message : "Failed to refresh resources"
			);
		} finally {
			setRediscovering(false);
		}
	};

  return (
    <div className="rounded-lg border border-slate-200 bg-white transition-shadow hover:shadow-sm">
      <div
        className="flex cursor-pointer items-start justify-between gap-4 p-4 hover:bg-slate-50"
        onClick={toggleExpanded}
      >
        <div className="flex min-w-0 items-start gap-3">
          <UserRound className="mt-0.5 h-4 w-4 shrink-0 text-slate-400" />
          <div className="min-w-0">
            <p className="mb-1 text-xs text-slate-500">Connected user · <code>end_user_ref</code></p>
            <p className="truncate font-mono text-sm font-medium text-slate-900">
              {connection.end_user_ref}
            </p>
            <p className="mt-0.5 truncate text-xs text-slate-500">
              {serviceName} · {connection.service_id.slice(0, 8)}
            </p>
          </div>
        </div>
        <div className="flex items-center gap-1">
          <div className="p-1.5 text-slate-400">
            {expanded ? (
              <ChevronUp className="h-4 w-4" />
            ) : (
              <ChevronDown className="h-4 w-4" />
            )}
          </div>
        </div>
      </div>
      {/* Keep copy outside the clickable header so it never expands the row or fetches resources. */}
      <div className="px-4 pb-4 pl-11">
        <AuthNameField name={connection.auth_name} context="bucket" />
      </div>
      {expanded && (
        <div className="border-t border-slate-100 bg-slate-50/50 p-4">
          <ConnectionMetaGrid
            connection={connection}
            serviceName={serviceName}
          />
          <ConnectionResources
            resources={resources}
            loading={loadingResources}
            onSetDefault={setDefaultResource}
			onRediscover={rediscoverResources}
			rediscovering={rediscovering}
            canManage={canManage}
          />
          {canManage && <div className="mt-4 flex justify-end">
            <button
              type="button"
              onClick={() => onRemoveConnection(connection)}
              disabled={deleting}
              className="flex items-center gap-2 rounded-md border border-red-200 bg-white px-3 py-1.5 text-xs font-medium text-red-600 shadow-sm hover:bg-red-50 hover:text-red-700 disabled:opacity-50"
            >
              {deleting ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <Trash2 className="h-3.5 w-3.5" />
              )}
              Remove Connection
            </button>
          </div>}
        </div>
      )}
    </div>
  );
}

// ConnectionResources shows the provider choices attached to this user and
// keeps the default command close to the routing metadata it changes.
function ConnectionResources({
  resources,
  loading,
  onSetDefault,
	onRediscover,
	rediscovering,
  canManage,
}: {
  resources: ConnectionResource[];
  loading: boolean;
  onSetDefault: (resourceId: string) => void;
	onRediscover: () => void;
	rediscovering: boolean;
  canManage: boolean;
}) {
  if (loading) {
    return (
      <div className="mt-4 text-xs text-slate-500">Loading resources...</div>
    );
  }
  return (
    <div className="mt-4 border-t border-slate-200 pt-3">
	  <div className="mb-2 flex items-center justify-between gap-2">
		<p className="text-xs font-medium text-slate-700">Provider resources</p>
		{canManage && <button
		  type="button"
		  onClick={onRediscover}
		  disabled={rediscovering}
		  title="Refresh provider resources"
		  className="rounded-md p-1 text-slate-500 hover:bg-slate-100 hover:text-slate-800 disabled:opacity-50"
		>
		  <RefreshCw className={`h-3.5 w-3.5 ${rediscovering ? "animate-spin" : ""}`} />
		</button>}
	  </div>
	  {resources.length === 0 ? (
		<p className="text-xs text-slate-400">No active resources.</p>
	  ) : (
      <div className="space-y-2">
        {resources.map((resource) => (
          <div
            key={resource.id}
            className="flex items-center justify-between gap-3 rounded-md border border-slate-200 bg-white px-3 py-2"
          >
            <div className="min-w-0">
              <p className="truncate text-xs font-medium text-slate-800">
                {resource.display_name || resource.provider_resource_id}
              </p>
              <p
                className="truncate font-mono text-[11px] text-slate-500"
                title={resource.base_url}
              >
                {resource.resource_type} · {resource.base_url}
              </p>
            </div>
            {resource.is_default ? (
              <span className="flex shrink-0 items-center gap-1 text-xs font-medium text-emerald-700">
                <MapPin className="h-3.5 w-3.5" />
                Default
              </span>
            ) : canManage ? (
              <button
                type="button"
                onClick={() => onSetDefault(resource.id)}
                className="shrink-0 text-xs font-medium text-blue-600 hover:text-blue-700"
              >
                Set default
              </button>
            ) : null}
          </div>
        ))}
      </div>
	  )}
    </div>
  );
}

function ConnectionMetaGrid({
  connection,
  serviceName,
}: {
  connection: AuthConnection;
  serviceName: string;
}) {
  const items = connectionMetaItems(connection, serviceName);
  return (
    <dl className="grid max-w-4xl grid-cols-2 gap-x-5 gap-y-2 sm:grid-cols-3 lg:grid-cols-4">
      {items.map((item) => (
        <div key={item.label} className="min-w-0">
          <dt className="text-[11px] font-medium uppercase text-slate-400">
            {item.label}
          </dt>
          <dd
            className="mt-0.5 truncate text-xs text-slate-700"
            title={item.value}
          >
            {item.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}

// connectionMetaItems keeps the expanded row declarative so adding lifecycle
// metadata cannot scatter rendering decisions through the table component.
function connectionMetaItems(
  connection: AuthConnection,
  serviceName: string
): Array<{ label: string; value: string }> {
  return [
    { label: "State", value: refreshStateLabel(connection.refresh_state) },
    {
      label: "Last Failure",
      value: failureSummary(connection),
    },
    {
      label: "Trace ID",
      value: connection.last_failure_trace_id || "-",
    },
    { label: "Auth", value: connection.auth_type || "Unknown" },
    { label: "Last Used", value: formatDateTime(connection.last_used_at) },
    { label: "Access Expires", value: formatDateTime(connection.expires_at) },
    {
      label: "Refresh Expires",
      value: formatDateTime(connection.refresh_token_expires_at),
    },
    { label: "Scopes", value: scopeSummary(connection) },
    { label: "Subject", value: connection.subject || "-" },
    { label: "Issuer", value: connection.issuer || serviceName },
  ];
}

// failureSummary combines the safe Engine code and timestamp so operators get
// useful context without seeing provider bodies, tokens, or user identifiers.
function failureSummary(connection: AuthConnection): string {
  // Connections without a recorded failure should remain visually quiet.
  if (!connection.last_failure_code) return "None";
  const occurredAt = formatDateTime(connection.last_failure_at);
  return `${failureCodeLabel(connection.last_failure_code)} · ${occurredAt}`;
}

// failureCodeLabel translates stable machine codes while preserving unknown
// future codes for forward-compatible debugging.
function failureCodeLabel(code: string): string {
  const labels: Record<string, string> = {
    provider_unauthorized: "Provider returned 401",
    provider_forbidden: "Provider returned 403",
    refresh_token_rejected: "Refresh grant rejected",
    refresh_token_expired: "Refresh token expired",
    refresh_token_missing: "Refresh token unavailable",
    refresh_failed_after_expiry: "Refresh failed after access expiry",
  };
  return labels[code] || code.replaceAll("_", " ");
}

// refreshStateLabel turns the Engine lifecycle value into an operator action
// without offering an admin reconnect that could authorize the wrong user.
function refreshStateLabel(state?: string): string {
  // A reconnect-required state is intentionally explicit because retries or
  // resource rediscovery cannot repair a revoked end-user grant.
  if (state === "reconnect_required") return "Reconnect required";
  // Empty state is legacy/incomplete data and should not be presented as a
  // healthy connection merely because it differs from reconnect_required.
  if (!state) return "Unknown";
  return state.charAt(0).toUpperCase() + state.slice(1);
}

function serviceName(services: ActivatedService[], serviceId: string): string {
  const service = services.find((item) => item.service_id === serviceId);
  return service?.service_name || serviceId.slice(0, 8);
}

function bucketServiceOptions(services: BucketServiceSummary[]) {
  return services.map((service) => ({
    id: service.service_id,
    label: serviceLabel(service),
  }));
}

function serviceLabel(service: BucketServiceSummary): string {
  return service.service_name
    ? `${service.service_name} · ${service.service_id.slice(0, 8)}`
    : service.service_id.slice(0, 8);
}

function scopeSummary(connection: AuthConnection): string {
  const count = connection.scopes?.length || 0;
  return count > 0
    ? `${count} from ${connection.scope_source || "provider"}`
    : "None";
}

function formatDateTime(value?: string): string {
  if (!value) return "Never";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "Unknown" : date.toLocaleString();
}
