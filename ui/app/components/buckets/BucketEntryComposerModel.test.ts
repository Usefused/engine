import assert from "node:assert/strict";
import test from "node:test";
import type { ServiceAuthOption } from "../../lib/service-auth";
import {
  type BucketSecretEntryForm,
  secretIsCredentialFamily,
  serializeBucketSecretPayload,
  serializeCredentialFamilyPayload,
  serializeSecretPayloads,
  serializeValuePayload,
  type SecretEntryForm,
  type ValueEntryForm,
  validateEntry,
} from "./BucketEntryComposerModel";

const validSecret: SecretEntryForm = {
  serviceId: "service-1",
  credentialType: "bearer:Authorization",
  value: "token",
  username: "",
  password: "",
  certificate: "",
  privateKey: "",
  clientId: "",
  clientSecret: "",
  expiresAt: "",
};

const validValue: ValueEntryForm = {
  serviceId: "service-1",
  keyName: "BASE_URL",
  location: "header",
  value: "https://api.example.test",
};

// Bucket secrets carry no service/credential-type qualifier, so an empty
// name and value are the only invalid states to guard against.
const validBucketSecret: BucketSecretEntryForm = {
  keyName: "api-key",
  value: "stored-value",
  expiresAt: "",
};

const bearerOption = authOption("bearer", ["value"], { id: "bearer:Authorization" });

test("validates required secret fields by credential type", () => {
  assert.equal(validateEntry("secret", { ...validSecret, serviceId: "" }, validValue, validBucketSecret, bearerOption), "Choose a service.");
  assert.equal(validateEntry("secret", validSecret, validValue, validBucketSecret), "Choose a credential type.");
  assert.equal(validateEntry("secret", { ...validSecret, username: "", password: "pass" }, validValue, validBucketSecret, authOption("basic", ["username", "password"])), "Enter the username.");
  assert.equal(validateEntry("secret", { ...validSecret, certificate: "cert", privateKey: "" }, validValue, validBucketSecret, authOption("mtls", ["certificate", "private_key"])), "Enter the private key.");
  assert.equal(validateEntry("secret", { ...validSecret, certificate: "cert", privateKey: "key" }, validValue, validBucketSecret, authOption("mtls", ["certificate", "private_key"], { key_prefix: "" })), "Credential metadata is missing its auth key prefix.");
});

test("validates oauth/oidc client_id and client_secret pairs", () => {
  const oauthOption = authOption("oauth", ["client_id", "client_secret"], { key_prefix: "oauthAuth" });
  assert.equal(validateEntry("secret", { ...validSecret, clientId: "", clientSecret: "s" }, validValue, validBucketSecret, oauthOption), "Enter the client ID.");
  assert.equal(validateEntry("secret", { ...validSecret, clientId: "id", clientSecret: "" }, validValue, validBucketSecret, oauthOption), "Enter the client secret.");
  assert.equal(validateEntry("secret", { ...validSecret, clientId: "id", clientSecret: "secret" }, validValue, validBucketSecret, oauthOption), "");
});

test("validates required env value fields", () => {
  assert.equal(validateEntry("value", validSecret, { ...validValue, keyName: "" }, validBucketSecret, bearerOption), "Enter a name.");
  assert.equal(validateEntry("value", validSecret, { ...validValue, value: "" }, validBucketSecret, bearerOption), "Enter a value.");
});

test("validates required bucket secret fields", () => {
  assert.equal(validateEntry("bucket_secret", validSecret, validValue, { ...validBucketSecret, keyName: "" }), "Enter a name.");
  assert.equal(validateEntry("bucket_secret", validSecret, validValue, { ...validBucketSecret, value: "" }), "Enter a value.");
  assert.equal(validateEntry("bucket_secret", validSecret, validValue, validBucketSecret), "");
});

test("serializes hidden secret key names from backend auth options", () => {
  assert.deepEqual(serializeSecretPayloads(validSecret, bearerOption), [{
    serviceId: "service-1",
    keyName: "Authorization",
    credentialType: "bearer",
    value: "token",
    expiresAt: undefined,
  }]);
  assert.equal(serializeSecretPayloads(validSecret, authOption("api_key", ["value"], { credential_type: "apiKey", key_name: "X-API-Key" }))[0].keyName, "X-API-Key");
  assert.equal(serializeSecretPayloads(validSecret, authOption("oauth", ["value"]))[0].credentialType, "oauth");
  assert.equal(serializeSecretPayloads(validSecret, authOption("oidc", ["value"]))[0].credentialType, "oidc");
});

test("serializes paired credentials as dispatcher-derived keys", () => {
  assert.deepEqual(serializeSecretPayloads({ ...validSecret, username: "user", password: "pass" }, authOption("basic", ["username", "password"], { key_prefix: "basicAuth" })).map((payload) => payload.keyName), [
    "basicAuth_username",
    "basicAuth_password",
  ]);
  assert.deepEqual(serializeSecretPayloads({ ...validSecret, certificate: "cert", privateKey: "key" }, authOption("mtls", ["certificate", "private_key"], { key_prefix: "mtls" })).map((payload) => payload.keyName), [
    "mtls_cert",
    "mtls_key",
  ]);
  assert.deepEqual(serializeSecretPayloads({ ...validSecret, certificate: "cert", privateKey: "key" }, authOption("mtls", ["certificate", "private_key"], { key_prefix: "clientCert" })).map((payload) => payload.keyName), [
    "clientCert_cert",
    "clientCert_key",
  ]);
});

test("serializes env values without leaking secret-only fields", () => {
  assert.deepEqual(serializeValuePayload({ ...validValue, keyName: " API_URL ", location: "" }), {
    serviceId: "service-1",
    keyName: "API_URL",
    location: "header",
    value: "https://api.example.test",
  });
});

test("serializes oauth/oidc client pairs as a credential_family payload", () => {
  const oauthOption = authOption("oauth", ["client_id", "client_secret"], { key_prefix: "githubAuth", credential_type: "oauth" });
  assert.deepEqual(
    serializeCredentialFamilyPayload({ ...validSecret, clientId: "abc", clientSecret: "xyz" }, oauthOption),
    {
      serviceId: "service-1",
      credentialType: "oauth",
      authName: "githubAuth",
      clientId: "abc",
      clientSecret: "xyz",
      expiresAt: undefined,
    }
  );
  assert.equal(secretIsCredentialFamily("oauth"), true);
  assert.equal(secretIsCredentialFamily("oidc"), true);
  assert.equal(secretIsCredentialFamily("basic"), false);
});

test("serializes a generic bucket secret without a service_id", () => {
  assert.deepEqual(serializeBucketSecretPayload({ ...validBucketSecret, keyName: " api-key " }), {
    keyName: "api-key",
    value: "stored-value",
    expiresAt: undefined,
  });
});

function authOption(authType: string, requiredFields: string[], overrides: Partial<ServiceAuthOption> = {}): ServiceAuthOption {
  return {
    id: `${authType}:test`,
    label: authType,
    auth_type: authType,
    credential_type: authType,
    key_name: "Authorization",
    key_prefix: authType,
    required_fields: requiredFields,
    supports_connected_users: authType === "oauth" || authType === "oidc",
    ...overrides,
  };
}
