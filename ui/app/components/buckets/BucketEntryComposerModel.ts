import type { ActivatedService, BucketValue, SecretMeta } from "../../lib/api";
import type { ServiceAuthOption } from "../../lib/service-auth";
import type {
  BucketSecretFormPayload,
  CredentialFamilyFormPayload,
} from "../../lib/buckets";

type BucketEntryKind = "secret" | "value" | "bucket_secret";

export type BucketEntryEditTarget =
  | { kind: "bucket_secret"; secret: SecretMeta }
  | { kind: "secret"; secret: SecretMeta; authOptionId: string }
  | { kind: "value"; value: BucketValue };

export type SecretFormPayload = {
  exportable?: boolean;
  serviceId: string;
  keyName: string;
  credentialType: string;
  value: string;
  expiresAt?: string;
};

export type ValueFormPayload = {
  serviceId: string;
  keyName: string;
  location: string;
  value: string;
};

export type SecretEntryForm = {
  exportable?: boolean;
  serviceId: string;
  credentialType: string;
  value: string;
  username: string;
  password: string;
  certificate: string;
  privateKey: string;
  clientId: string;
  clientSecret: string;
  expiresAt: string;
};

export type ValueEntryForm = {
  serviceId: string;
  keyName: string;
  location: string;
  value: string;
};

// A bucket secret has no service selection or credential-type qualifier, so
// it gets its own minimal form shape instead of reusing SecretEntryForm.
export type BucketSecretEntryForm = {
  exportable?: boolean;
  keyName: string;
  value: string;
  expiresAt: string;
};

export function validateEntry(kind: BucketEntryKind, secret: SecretEntryForm, value: ValueEntryForm, bucketSecret: BucketSecretEntryForm, authOption?: ServiceAuthOption): string {
  if (kind === "secret") return validateSecret(secret, authOption);
  if (kind === "bucket_secret") return validateBucketSecret(bucketSecret);
  return validateValue(value);
}

/** Only API-key selections may carry explicit retrieval consent into provider writes. */
export function serializeSecretPayloads(form: SecretEntryForm, authOption: ServiceAuthOption): SecretFormPayload[] {
  if (authOption.auth_type === "basic") return pairedSecretPayloads(form, authOption, [
    ["username", form.username],
    ["password", form.password],
  ]);
  if (authOption.auth_type === "mtls") return pairedSecretPayloads(form, authOption, [
    ["cert", form.certificate],
    ["key", form.privateKey],
  ]);
  return [{
    serviceId: form.serviceId.trim(),
    keyName: authOption.key_name,
    credentialType: authOption.credential_type,
    value: form.value,
    exportable: authOption.auth_type === "api_key" && form.exportable === true,
    expiresAt: serializedExpiry(form),
  }];
}

export function serializeValuePayload(form: ValueEntryForm): ValueFormPayload {
  return {
    serviceId: form.serviceId.trim(),
    keyName: form.keyName.trim(),
    location: form.location.trim() || "header",
    value: form.value,
  };
}

// OAuth/OIDC application pairs use Engine's semantic credential_family write
// path (auth_name + client_id + client_secret) instead of the generic
// two-row paired payload Basic/mTLS use, because Engine alone derives their
// storage key names.
export function serializeCredentialFamilyPayload(form: SecretEntryForm, authOption: ServiceAuthOption): CredentialFamilyFormPayload {
  return {
    serviceId: form.serviceId.trim(),
    credentialType: authOption.credential_type,
    authName: authOption.key_prefix.trim(),
    clientId: form.clientId,
    clientSecret: form.clientSecret,
    expiresAt: serializedExpiry(form),
  };
}

/** Generic secrets default to private unless the user explicitly permits retrieval. */
export function serializeBucketSecretPayload(form: BucketSecretEntryForm): BucketSecretFormPayload {
  return {
    keyName: form.keyName.trim(),
    exportable: form.exportable === true,
    value: form.value,
    expiresAt: form.expiresAt ? new Date(form.expiresAt).toISOString() : undefined,
  };
}

// Distinguishes the oauth/oidc credential_family write path from the
// row-oriented paired path Basic/mTLS still use, even though both render as
// a two-field form.
export function secretIsCredentialFamily(credentialType: string): boolean {
  return credentialType === "oauth" || credentialType === "oidc";
}

export function tokenPlaceholder(credentialType: string): string {
  if (credentialType === "oauth") return "OAuth access token";
  if (credentialType === "oidc") return "OIDC token";
  if (credentialType === "api_key") return "API key value";
  return "Bearer token";
}

function validateSecret(form: SecretEntryForm, authOption?: ServiceAuthOption): string {
  if (!form.serviceId.trim()) return "Choose a service.";
  if (!authOption) return "Choose a credential type.";
  if (authOption.required_fields.includes("username")) return validatePairedAuthSecret(authOption, form.username, form.password, "Enter the username.", "Enter the password.");
  if (authOption.required_fields.includes("certificate")) return validatePairedAuthSecret(authOption, form.certificate, form.privateKey, "Enter the certificate.", "Enter the private key.");
  if (authOption.required_fields.includes("client_id")) return validatePairedAuthSecret(authOption, form.clientId, form.clientSecret, "Enter the client ID.", "Enter the client secret.");
  if (authOption.required_fields.includes("value") && !form.value.trim()) return `Enter the ${tokenLabel(authOption.auth_type)}.`;
  return "";
}

// Bucket secrets have no service or credential type to validate -- just a
// non-empty reference-safe name and a non-empty value.
function validateBucketSecret(form: BucketSecretEntryForm): string {
  if (!form.keyName.trim()) return "Enter a name.";
  if (!form.value.trim()) return "Enter a value.";
  return "";
}

function validatePairedAuthSecret(authOption: ServiceAuthOption, left: string, right: string, leftMessage: string, rightMessage: string): string {
  // Paired auth keys must line up with Engine's runtime lookup convention;
  // missing metadata would otherwise save unusable names like "_cert".
  if (!authOption.key_prefix.trim()) return "Credential metadata is missing its auth key prefix.";
  return validatePairedSecret(left, right, leftMessage, rightMessage);
}

function validatePairedSecret(left: string, right: string, leftMessage: string, rightMessage: string): string {
  if (!left.trim()) return leftMessage;
  if (!right.trim()) return rightMessage;
  return "";
}

function validateValue(form: ValueEntryForm): string {
  if (!form.serviceId.trim()) return "Choose a service.";
  if (!form.keyName.trim()) return "Enter a name.";
  if (!form.value.trim()) return "Enter a value.";
  return "";
}

function pairedSecretPayloads(form: SecretEntryForm, authOption: ServiceAuthOption, fields: Array<[string, string]>): SecretFormPayload[] {
  // The dispatcher resolves paired credential families by derived bucket keys.
  // Writing both derived keys from one user action prevents intentional partial auth.
  const keyPrefix = authOption.key_prefix.trim();
  return fields.map(([suffix, value]) => ({
    serviceId: form.serviceId.trim(),
    credentialType: authOption.credential_type,
    keyName: `${keyPrefix}_${suffix}`,
    value,
    expiresAt: serializedExpiry(form),
  }));
}

function tokenLabel(credentialType: string): string {
  if (credentialType === "oauth") return "OAuth token";
  if (credentialType === "oidc") return "OIDC token";
  if (credentialType === "api_key") return "API key value";
  return "token";
}

function serializedExpiry(form: SecretEntryForm): string | undefined {
  return form.expiresAt ? new Date(form.expiresAt).toISOString() : undefined;
}

/** Formats an ISO timestamp for datetime-local inputs without inventing a date when missing or invalid. */
export function formatDateTimeLocalInput(iso?: string | null): string {
  // Blank expiry means the secret never expires.
  if (!iso) return "";
  const date = new Date(iso);
  // Ignore unparseable timestamps rather than producing NaN input values.
  if (Number.isNaN(date.getTime())) return "";
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** Resolves whether an existing bucket secret row can be updated in place through the existing upsert APIs. */
export function bucketSecretEditTarget(secret: SecretMeta, services: ActivatedService[]): BucketEntryEditTarget | null {
  // Generic bucket secrets are service-independent and always editable by key name.
  if (secret.credential_type === "bucket_secret") {
    return { kind: "bucket_secret", secret };
  }
  const service = services.find((item) => item.service_id === secret.service_id);
  const option = service?.auth_options?.find((item) => item.key_name === secret.key_name || item.key_prefix === secret.key_name);
  // Service auth updates require the matching auth scheme metadata so the upsert targets the exact stored keys.
  if (!service || !option) return null;
  return { kind: "secret", secret, authOptionId: option.id };
}

/** Builds pre-filled composer drafts for in-place updates while keeping stored secret material write-only. */
export function editTargetInitialDraft(target: BucketEntryEditTarget): {
  secret?: SecretEntryForm;
  value?: ValueEntryForm;
  bucketSecret?: BucketSecretEntryForm;
} {
  // Generic bucket secrets strip the internal "secret:" storage prefix so upsertBucketSecret targets the exact same key.
  if (target.kind === "bucket_secret") {
    return {
      bucketSecret: {
        keyName: target.secret.key_name.replace(/^secret:/, ""),
        value: "",
        exportable: target.secret.exportable === true,
        expiresAt: formatDateTimeLocalInput(target.secret.expires_at),
      },
    };
  }
  // Service auth updates lock the existing service/scheme and retain policy/expiry while leaving secret inputs blank.
  if (target.kind === "secret") {
    return {
      secret: {
        serviceId: target.secret.service_id,
        credentialType: target.authOptionId,
        value: "",
        username: "",
        password: "",
        certificate: "",
        privateKey: "",
        clientId: "",
        clientSecret: "",
        exportable: target.secret.exportable === true,
        expiresAt: formatDateTimeLocalInput(target.secret.expires_at),
      },
    };
  }
  return {
    value: {
      serviceId: target.value.service_id,
      keyName: target.value.key_name,
      location: target.value.location || "header",
      value: target.value.value,
    },
  };
}
