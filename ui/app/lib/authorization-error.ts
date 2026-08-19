export interface PermissionRequirement {
  permission: string;
  resource_type: string;
  resource_id: string;
  display_name?: string;
}

export interface APIErrorPayload {
  error?: string;
  code?: string;
  category?: string;
  retryable?: boolean;
  details?: Record<string, unknown>;
  remediation?: string;
  trace_id?: string;
  message?: string;
  missing?: PermissionRequirement[];
}

export class APIRequestError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly category?: string;
  readonly retryable: boolean;
  readonly details: Record<string, unknown>;
  readonly remediation?: string;
  readonly traceId?: string;
  readonly missing: PermissionRequirement[];

  constructor(status: number, payload: APIErrorPayload) {
    super(apiErrorMessage(status, payload));
    this.name = "APIRequestError";
    this.status = status;
    this.code = payload.code || payload.error;
    this.category = payload.category;
    this.retryable = payload.retryable === true;
    this.details = payload.details || {};
    this.remediation = payload.remediation;
    this.traceId = payload.trace_id;
    this.missing = Array.isArray(payload.missing) ? payload.missing : [];
  }
}

function asString(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function asBoolean(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

export function normalizeAPIErrorPayload(input: unknown): APIErrorPayload {
  if (!isRecord(input)) return {};
  if (isRecord(input.error)) {
    const engineError = input.error;
    return {
      error: asString(engineError.message),
      code: asString(engineError.code),
      category: asString(engineError.category),
      retryable: asBoolean(engineError.retryable),
      details: isRecord(engineError.details) ? engineError.details : undefined,
      remediation: asString(engineError.remediation),
      trace_id: asString(engineError.trace_id),
    };
  }
  return {
    error: asString(input.error),
    message: asString(input.message),
    missing: normalizeMissingRequirements(input.missing),
  };
}

export function apiErrorMessage(
  status: number,
  payload: APIErrorPayload
): string {
  const specific = specificApiErrorMessage(status, payload);
  if (specific) return specific;
  const coded = codedErrorMessage(payload);
  if (coded) return coded;
  return genericStatusMessage(status);
}

function specificApiErrorMessage(
  status: number,
  payload: APIErrorPayload
): string | null {
  const code = payload.code || payload.error;
  if (status === 401 && code === "authentication_required") {
    return "Authentication required. Provide a valid Fused credential.";
  }
  if (status === 403 && code === "permission_denied") {
    return permissionDeniedMessage(payload.missing);
  }
  return appOwnerErrorMessage(payload.error) || workspaceConfigErrorMessage(payload);
}

function codedErrorMessage(payload: APIErrorPayload): string | null {
  if (!payload.code || !payload.error) return null;
  return payload.remediation
    ? `${payload.error} ${payload.remediation}`
    : payload.error;
}

export function isAuthenticationFailure(
  status: number,
  payload: APIErrorPayload
): boolean {
  if (status !== 401) return false;
  const code = payload.code || payload.error;
  return [
    "authentication_required",
    "invalid API key",
    "missing X-API-Key header",
  ].includes(code || "");
}

// isImportVersionRequired keys UI recovery off the Registry's stable code so
// wording changes cannot suppress the version input again.
export function isImportVersionRequired(error: unknown): boolean {
  return error instanceof APIRequestError && error.code === "import_version_required";
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
      : ["service", "bucket", "app"].includes(resourceType)
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
    "app.read": "view",
    "app.create": "create an SDK or MCP server in",
    "app.manage": "manage",
    "app.tokens.manage": "manage",
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

function appOwnerErrorMessage(code: string | undefined): string | null {
  switch (code) {
    case "app owner is immutable":
      return "This resource already has an owner. Keep its existing personal or team ownership.";
    case "app owner is unavailable":
      return "The resource owner is no longer available. Ask a workspace administrator for help.";
    case "owner team was not found or is archived":
      return "Choose an active team, or use personal ownership.";
    case "app owner authorization denied":
      return "You or the owning team no longer have the access required to complete this action.";
    default:
      return null;
  }
}

function workspaceConfigErrorMessage(payload: APIErrorPayload): string | null {
  const code = payload.code || payload.error;
  if (!code) return null;

  if (code === "bucket_credentials_missing") {
    return bucketCredentialsMissingMessage(payload);
  }
  if (code === "credential_set_required") {
    return "Choose one credential set before creating this consumer.";
  }
  if (code === "credential_set_not_found") {
    return "The selected credential set no longer exists. Choose another credential set.";
  }
  return null;
}

function bucketCredentialsMissingMessage(payload: APIErrorPayload): string {
  const missing = Array.isArray(payload.details?.missing)
    ? payload.details.missing.filter((value): value is string => typeof value === "string")
    : [];
  const requirements = uniqueBucketMaterialLabels(missing);
  const message = requirements.length > 0
    ? `The selected credential set is missing ${requirements.join(", ")}.`
    : payload.error || "The selected credential set is missing required authentication.";
  return payload.remediation ? `${message} ${payload.remediation}` : message;
}

function uniqueBucketMaterialLabels(requirements: string[]): string[] {
  return requirements
    .map(bucketMaterialLabel)
    .filter((value, index, values) => value && values.indexOf(value) === index);
}

function bucketMaterialLabel(requirement: string): string {
  const match = requirement.match(/^[0-9a-f-]+ \(([^)]+)\)$/i);
  const material = match?.[1]?.trim();
  if (!material) return "required authentication";
  if (material === "oauth") return "an OAuth connection";
  if (material === "oidc") return "an OpenID Connect connection";

  const separator = material.indexOf(":");
  if (separator === -1) return "required authentication";
  const authType = material.slice(0, separator);
  const key = material.slice(separator + 1);
  const authLabel: Record<string, string> = {
    api_key: "API key",
    basic: "Basic auth",
    bearer: "bearer token",
    mtls: "mTLS",
  };
  return key === "<credential-name>"
    ? `${authLabel[authType] || "authentication"} credentials`
    : `${authLabel[authType] || "authentication"} credential ${key}`;
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
