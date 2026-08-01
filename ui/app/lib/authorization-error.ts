export interface PermissionRequirement {
  permission: string;
  resource_type: string;
  resource_id: string;
  display_name?: string;
}

export interface APIErrorPayload {
  error?: string;
  message?: string;
  missing?: PermissionRequirement[];
}

export class APIRequestError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly missing: PermissionRequirement[];

  constructor(status: number, payload: APIErrorPayload) {
    super(apiErrorMessage(status, payload));
    this.name = "APIRequestError";
    this.status = status;
    this.code = payload.error;
    this.missing = Array.isArray(payload.missing) ? payload.missing : [];
  }
}

export function normalizeAPIErrorPayload(input: unknown): APIErrorPayload {
  if (!isRecord(input)) return {};
  return {
    error: typeof input.error === "string" ? input.error : undefined,
    message: typeof input.message === "string" ? input.message : undefined,
    missing: normalizeMissingRequirements(input.missing),
  };
}

export function apiErrorMessage(
  status: number,
  payload: APIErrorPayload
): string {
  if (status === 401 && payload.error === "authentication_required") {
    return "Authentication required. Provide a valid Fused credential.";
  }
  if (status === 403 && payload.error === "permission_denied") {
    return permissionDeniedMessage(payload.missing);
  }
  const ownerMessage = artifactOwnerErrorMessage(payload.error);
  if (ownerMessage) return ownerMessage;
  return genericStatusMessage(status);
}

export function isAuthenticationFailure(
  status: number,
  payload: APIErrorPayload
): boolean {
  if (status !== 401) return false;
  return [
    "authentication_required",
    "invalid API key",
    "missing X-API-Key header",
  ].includes(payload.error || "");
}

function permissionDeniedMessage(
  missing: PermissionRequirement[] | undefined
): string {
  if (!Array.isArray(missing) || missing.length === 0) {
    return "Permission denied. Ask a workspace administrator for access.";
  }
  if (missing.some((requirement) => requirement.permission === "access.manage")) {
    return "You are not a member of the owning team. Join that team or ask an access administrator to perform this action.";
  }
  return `You or the owning team need access to ${missing
    .map(productPermissionRequirementLabel)
    .join("; ")}.`;
}

function productPermissionRequirementLabel(
  requirement: PermissionRequirement
): string {
  const displayName = requirement.display_name?.trim();
  const resourceType = requirement.resource_type.trim();
  const resource = displayName
    ? `${resourceType} "${displayName}"`
    : resourceType === "workspace"
      ? "this workspace"
      : ["service", "bucket", "artifact"].includes(resourceType)
        ? `the selected ${resourceType}`
        : "the requested resource";
  return `${productPermissionAction(requirement.permission)} ${resource}`;
}

function productPermissionAction(permission: string): string {
  const actions: Record<string, string> = {
    "workspace.read": "view",
    "workspace.update": "change",
    "service.read": "view",
    "service.consume": "use",
    "service.manage": "manage",
    "bucket.read": "view",
    "bucket.values.read": "view",
    "bucket.use": "use",
    "bucket.manage": "manage",
    "artifact.read": "view",
    "artifact.create": "create an SDK, MCP server, or webhook in",
    "artifact.manage": "manage",
    "artifact.tokens.manage": "manage",
    "connection.read": "view connections in",
    "connection.manage": "manage connections in",
    "audit.read": "view access activity for",
  };
  return actions[permission.trim()] || "complete this action for";
}

export function advancedPermissionDiagnostics(requirements: PermissionRequirement[]): string[] {
  return requirements.map((requirement) =>
    `${requirement.permission.trim()} on ${requirement.resource_type.trim()} (${requirement.resource_id.trim()})`
  );
}

function artifactOwnerErrorMessage(code: string | undefined): string | null {
  switch (code) {
    case "owner_team_id is required for a new artifact":
      return "Choose an owning team before creating this SDK, MCP server, or webhook.";
    case "artifact owner team is immutable":
    case "sdk scope owner mismatch":
      return "This artifact already belongs to another team. Choose its existing owning team; ownership cannot be changed during apply.";
    case "artifact owner is unavailable":
      return "The artifact's owning team is no longer available. Ask an access administrator for help.";
    case "artifact owner authorization denied":
      return "You or the owning team no longer have the access required to complete this action.";
    default:
      return null;
  }
}

function genericStatusMessage(status: number): string {
  switch (status) {
    case 400: return "Fused could not use this request. Check the selected team and inputs.";
    case 403: return "This action is not available with your current workspace access.";
    case 404: return "The requested workspace resource was not found.";
    case 409: return "The workspace changed since this action was planned. Refresh and try again.";
    default: return `Fused could not complete the request (HTTP ${status}).`;
  }
}

function normalizeMissingRequirements(input: unknown): PermissionRequirement[] {
  if (!Array.isArray(input)) return [];
  return input.flatMap((item) => {
    if (!isRecord(item)) return [];
    if (
      typeof item.permission !== "string" ||
      typeof item.resource_type !== "string" ||
      typeof item.resource_id !== "string" ||
      !item.permission.trim() ||
      !item.resource_type.trim() ||
      !item.resource_id.trim()
    ) {
      return [];
    }
    return [{
      permission: item.permission,
      resource_type: item.resource_type,
      resource_id: item.resource_id,
      display_name:
        typeof item.display_name === "string" ? item.display_name : undefined,
    }];
  });
}

function isRecord(input: unknown): input is Record<string, unknown> {
  return typeof input === "object" && input !== null && !Array.isArray(input);
}
