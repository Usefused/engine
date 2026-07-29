import type { ServiceAuthOption } from "../../lib/service-auth";

type BucketEntryKind = "secret" | "value";

export type SecretFormPayload = {
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
  serviceId: string;
  credentialType: string;
  value: string;
  username: string;
  password: string;
  certificate: string;
  privateKey: string;
  expiresAt: string;
};

export type ValueEntryForm = {
  serviceId: string;
  keyName: string;
  location: string;
  value: string;
};

export function validateEntry(kind: BucketEntryKind, secret: SecretEntryForm, value: ValueEntryForm, authOption?: ServiceAuthOption): string {
  if (kind === "secret") return validateSecret(secret, authOption);
  return validateValue(value);
}

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

export function secretHasTwoFields(credentialType: string): boolean {
  return credentialType === "basic" || credentialType === "mtls";
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
  if (authOption.required_fields.includes("value") && !form.value.trim()) return `Enter the ${tokenLabel(authOption.auth_type)}.`;
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
