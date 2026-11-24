import { FieldLabel } from "~/components/forms/FieldLabel";
import { Select } from "../forms/Select.ts";
import { type Dispatch, type FormEvent, type SetStateAction, useEffect, useState } from "react";
import { Check } from "lucide-react";
import { type ActivatedService } from "~/lib/api";
import type { ServiceAuthOption } from "~/lib/service-auth";
import { BucketServiceSelect } from "~/components/buckets/BucketServiceSelect";
import {
  type BucketEntryKind,
  type BucketSecretFormPayload,
  type CredentialFamilyFormPayload,
  type SecretFormPayload,
  type ValueFormPayload,
} from "~/lib/buckets";
import {
  type BucketEntryEditTarget,
  type BucketSecretEntryForm,
  editTargetInitialDraft,
  secretIsCredentialFamily,
  serializeBucketSecretPayload,
  serializeCredentialFamilyPayload,
  serializeSecretPayloads,
  serializeValuePayload,
  type SecretEntryForm,
  tokenPlaceholder,
  validateEntry,
  type ValueEntryForm,
} from "~/components/buckets/BucketEntryComposerModel";

type BucketEntryComposerProps = {
  kind: BucketEntryKind | null;
  editTarget?: BucketEntryEditTarget | null;
  saving: boolean;
  services: ActivatedService[];
  onCancel: () => void;
  onSaveSecret: (payload: SecretFormPayload) => Promise<void>;
  onSaveSecrets: (payloads: SecretFormPayload[]) => Promise<void>;
  onSaveCredentialFamily: (payload: CredentialFamilyFormPayload) => Promise<void>;
  onSaveValue: (payload: ValueFormPayload) => Promise<void>;
  onSaveBucketSecret: (payload: BucketSecretFormPayload) => Promise<void>;
};

const emptySecret: SecretEntryForm = {
  serviceId: "",
  credentialType: "",
  value: "",
  username: "",
  password: "",
  certificate: "",
  privateKey: "",
  clientId: "",
  clientSecret: "",
  expiresAt: "",
};

const emptyValue: ValueEntryForm = {
  serviceId: "",
  keyName: "",
  location: "header",
  value: "",
};

// A bucket secret is service-independent, so it never needs a service_id.
const emptyBucketSecret: BucketSecretEntryForm = {
  keyName: "",
  value: "",
  expiresAt: "",
};

/** Resolves the active service id and matching auth option for the current composer draft. */
function resolveComposerSelection(
  kind: BucketEntryKind,
  services: ActivatedService[],
  secret: SecretEntryForm,
  value: ValueEntryForm
) {
  // Secret and env-value drafts store their service selection in separate form states.
  const serviceId = kind === "secret" ? secret.serviceId : value.serviceId;
  const selectedService = services.find((service) => service.service_id === serviceId);
  // Only service-bound credentials expose authentication scheme options.
  const authOptions = kind === "secret" ? serviceAuthOptions(selectedService) : [];
  const selectedAuthOption = authOptions.find((option) => option.id === secret.credentialType);
  return { serviceId, authOptions, selectedAuthOption };
}

/** Syncs composer drafts when the composer closes or opens an existing entry for editing. */
function syncComposerDraft(
  effectiveKind: BucketEntryKind | null,
  editTarget: BucketEntryEditTarget | null,
  setSecret: Dispatch<SetStateAction<SecretEntryForm>>,
  setValue: Dispatch<SetStateAction<ValueEntryForm>>,
  setBucketSecret: Dispatch<SetStateAction<BucketSecretEntryForm>>,
  setServiceSearch: Dispatch<SetStateAction<string>>,
  setError: Dispatch<SetStateAction<string>>
) {
  // Closing both add and edit flows clears any unsaved draft state.
  if (!effectiveKind) {
    resetForms(setSecret, setValue, setBucketSecret, setServiceSearch, setError);
    return;
  }
  // Opening an existing row pre-fills its immutable identity and editable settings while keeping secrets write-only.
  if (editTarget) {
    const initial = editTargetInitialDraft(editTarget);
    setSecret(initial.secret ?? emptySecret);
    setValue(initial.value ?? emptyValue);
    setBucketSecret(initial.bucketSecret ?? emptyBucketSecret);
    setServiceSearch("");
    setError("");
  }
}

/** Keeps credential fields responsive and places explicit save/cancel actions below the form. */
export function BucketEntryComposer({
  kind,
  editTarget = null,
  saving,
  services,
  onCancel,
  onSaveSecret,
  onSaveSecrets,
  onSaveCredentialFamily,
  onSaveValue,
  onSaveBucketSecret,
}: BucketEntryComposerProps) {
  const [secret, setSecret] = useState(emptySecret);
  const [value, setValue] = useState(emptyValue);
  const [bucketSecret, setBucketSecret] = useState(emptyBucketSecret);
  const [serviceSearch, setServiceSearch] = useState("");
  const [error, setError] = useState("");
  // Active edit targets determine the form kind even when the add dropdown is closed.
  const effectiveKind = editTarget?.kind ?? kind;
  const isEditing = editTarget !== null;

  useEffect(() => {
    syncComposerDraft(
      effectiveKind,
      editTarget,
      setSecret,
      setValue,
      setBucketSecret,
      setServiceSearch,
      setError
    );
  }, [effectiveKind, editTarget]);

  useEffect(() => {
    // Only fresh add flows auto-select a default service; edits preserve their target service.
    if (effectiveKind && !editTarget)
      applyDefaultService(
        effectiveKind,
        services,
        setSecret,
        setValue,
        setServiceSearch
      );
  }, [effectiveKind, editTarget, services]);

  // Render nothing while neither an add flow nor an in-place edit is open.
  if (!effectiveKind) return null;

  const { serviceId, authOptions, selectedAuthOption } = resolveComposerSelection(
    effectiveKind,
    services,
    secret,
    value
  );

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const validationError = validateEntry(
      effectiveKind,
      secret,
      value,
      bucketSecret,
      selectedAuthOption
    );
    if (validationError) {
      setError(validationError);
      return;
    }
    try {
      await saveEntry(
        effectiveKind,
        secret,
        value,
        bucketSecret,
        selectedAuthOption,
        onSaveSecret,
        onSaveSecrets,
        onSaveCredentialFamily,
        onSaveValue,
        onSaveBucketSecret
      );
      onCancel();
    } catch {
      // Route-level save helpers own the toast so the composer can stay focused
      // on form state while still keeping failed submissions open.
    }
  };

  return (
    <form onSubmit={submit} className="@container border-b border-slate-100 px-6 py-4">
      {/* Identify in-place updates clearly so users know the existing entry will be overwritten. */}
      {isEditing && (
        <p className="mb-3 text-xs font-medium text-slate-600">
          Updating existing {composerKindLabel(effectiveKind)} — saving replaces the stored value.
        </p>
      )}
      {/* Use the form width, not the browser width: one column on phones and at most two in a wide drawer. */}
      <div
        className="grid grid-cols-1 items-end gap-4 @min-[30rem]:grid-cols-2"
      >
        {/* Generic bucket secrets are service-independent and omit service and qualifier selectors. */}
        {effectiveKind !== "bucket_secret" && (
          <>
            <ComposerServiceSelect
              services={services}
              search={serviceSearch}
              value={serviceId}
              disabled={isEditing}
              onSearchChange={setServiceSearch}
              onChange={(next) => {
                setError("");
                updateServiceID(effectiveKind, next, services, setSecret, setValue);
              }}
            />
            <QualifierSelect
              kind={effectiveKind}
              authOptions={authOptions}
              secret={secret}
              value={value}
              disabled={isEditing && effectiveKind === "secret"}
              setSecret={setSecret}
              setValue={setValue}
            />
          </>
        )}
        <EntryValueFields
          kind={effectiveKind}
          lockIdentity={isEditing}
          authOption={selectedAuthOption}
          secret={secret}
          value={value}
          bucketSecret={bucketSecret}
          setSecret={setSecret}
          setValue={setValue}
          setBucketSecret={setBucketSecret}
        />
        <BucketSecretOptions kind={effectiveKind} authType={selectedAuthOption?.auth_type} secret={secret} bucketSecret={bucketSecret} setSecret={setSecret} setBucketSecret={setBucketSecret} saving={saving} />
      </div>
      {/* Validation stays adjacent to the actions so a failed save explains how to proceed. */}
      {error && (
        <p role="alert" className="mt-3 text-xs font-medium text-red-600">{error}</p>
      )}
      <BucketEntryActions kind={effectiveKind} isEditing={isEditing} saving={saving} hasServices={services.length > 0} onCancel={onCancel} />
    </form>
  );
}

/** Keeps labeled actions below every field, independent of the responsive field grid. */
function BucketEntryActions({ kind, isEditing, saving, hasServices, onCancel }: {
  kind: BucketEntryKind;
  isEditing: boolean;
  saving: boolean;
  hasServices: boolean;
  onCancel: () => void;
}) {
  const actionVerb = isEditing ? "Update" : "Save";
  return <>
      {/* A separate action row stays below all fields at every drawer width. */}
      <div className="mt-4 flex flex-wrap items-center justify-end gap-2 border-t border-slate-100 pt-4">
        <button
          type="button"
          onClick={onCancel}
          disabled={saving}
          className="inline-flex min-h-10 items-center justify-center rounded-lg border border-slate-200 bg-white px-4 py-2 text-sm font-medium text-slate-600 hover:bg-slate-50 disabled:opacity-40"
          aria-label="Cancel entry"
        >
          Cancel
        </button>
        <button
          type="submit"
          // Service-scoped entries require an available service; generic secrets do not.
          disabled={saving || (kind !== "bucket_secret" && !hasServices)}
          className="inline-flex min-h-10 items-center justify-center gap-2 rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-40"
          aria-label={`${actionVerb} ${composerKindLabel(kind)}`}
        >
          <Check className="h-4 w-4" aria-hidden="true" />
          {/* Pending feedback prevents repeated submissions while retaining the action's purpose. */}
          {saving ? "Saving…" : `${actionVerb} ${composerKindLabel(kind)}`}
        </button>
      </div>
  </>;
}

/** Keeps optional retrieval and expiry settings together without mixing them into form actions. */
function BucketSecretOptions({ kind, authType, secret, bucketSecret, setSecret, setBucketSecret, saving }: {
  kind: BucketEntryKind;
  authType?: string;
  secret: SecretEntryForm;
  bucketSecret: BucketSecretEntryForm;
  setSecret: Dispatch<SetStateAction<SecretEntryForm>>;
  setBucketSecret: Dispatch<SetStateAction<BucketSecretEntryForm>>;
  saving: boolean;
}) {
  // Environment values are resolved live and have neither expiry nor secret retrieval policy.
  if (kind === "value") return null;
  // Generic and provider secrets retain independent drafts when the creation kind changes.
  const current = kind === "bucket_secret" ? bucketSecret : secret;
  /** Updates only the active credential draft while preserving the remaining fields. */
  function update(patch: { exportable?: boolean; expiresAt?: string }) {
    // Service-independent credentials must never overwrite provider credential state.
    if (kind === "bucket_secret") setBucketSecret(/** Merge the optional setting without clearing typed values. */ (previous) => ({ ...previous, ...patch }));
    else setSecret(/** Preserve provider selection and all authentication fields. */ (previous) => ({ ...previous, ...patch }));
  }
  return <>
    {/* Retrieval opt-in is supported only by generic secrets and provider API keys. */}
    {(kind === "bucket_secret" || authType === "api_key") && <label className="flex items-start gap-2 text-xs text-slate-600">
      <input type="checkbox" disabled={saving} checked={current.exportable === true}
        onChange={/** Apply the explicit retrieval choice only to this draft. */ (event) => update({ exportable: event.target.checked })} />
      <span>Allow secret retrieval<br /><span className="text-slate-400">Callers with reveal permission can use this key outside Fused.</span></span>
    </label>}
    <ExpiryInput value={current.expiresAt} onChange={/** Retain optional expiration without changing the credential's value. */ (expiresAt) => update({ expiresAt })} />
  </>;
}

/**
 * Maps the internal entry-kind literal to the display wording used in the Add
 * dropdown ("Service Auth" / "Secret" / "Env") so the Save button's
 * accessible name matches what the user picked, instead of leaking the raw
 * "bucket_secret" kind literal into assistive text.
 */
function composerKindLabel(kind: BucketEntryKind): string {
  if (kind === "secret") return "service auth";
  if (kind === "bucket_secret") return "secret";
  return "env value";
}

/** A credential or stored value always targets one selected service. */
function ComposerServiceSelect({
  services,
  search,
  value,
  disabled = false,
  onSearchChange,
  onChange,
}: {
  services: ActivatedService[];
  search: string;
  value: string;
  disabled?: boolean;
  onSearchChange: (search: string) => void;
  onChange: (value: string) => void;
}) {
  // Lock the service field during in-place edits so updates overwrite the exact selected entry.
  if (disabled) {
    const current = services.find((service) => service.service_id === value);
    return (
      <label className="min-w-0">
        <span className="mb-1 block text-xs font-medium text-slate-500">Service</span>
        <input
          type="text"
          disabled
          value={current ? serviceLabel(current) : value}
          className="h-[38px] w-full rounded-lg border border-slate-200 bg-slate-50 px-3 text-sm text-slate-500"
        />
      </label>
    );
  }
  return (
    <div className="min-w-0">
      <BucketServiceSelect
        id="bucket-entry-service"
        required
        label="Service"
        placeholder={
          services.length === 0 ? "No workspace services" : "Search services"
        }
        options={services.map((service) => ({
          id: service.service_id,
          label: serviceLabel(service),
        }))}
        search={search}
        selectedServiceId={value}
        className="w-full"
        buttonClassName="h-[38px]"
        onSearchChange={onSearchChange}
        onSelectedServiceChange={onChange}
      />
    </div>
  );
}

/** Selects the value fields for the active entry kind while locking key identity during in-place edits. */
function EntryValueFields({
  kind,
  lockIdentity = false,
  authOption,
  secret,
  value,
  bucketSecret,
  setSecret,
  setValue,
  setBucketSecret,
}: {
  kind: BucketEntryKind;
  lockIdentity?: boolean;
  authOption?: ServiceAuthOption;
  secret: typeof emptySecret;
  value: typeof emptyValue;
  bucketSecret: typeof emptyBucketSecret;
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void;
  setValue: (updater: (prev: typeof emptyValue) => typeof emptyValue) => void;
  setBucketSecret: (
    updater: (prev: typeof emptyBucketSecret) => typeof emptyBucketSecret
  ) => void;
}) {
  if (kind === "secret") {
    return (
      <SecretValueFields
        authOption={authOption}
        secret={secret}
        setSecret={setSecret}
      />
    );
  }
  // A bucket secret is service-independent: it only ever needs a name and a
  // value, never a service selector or credential-type qualifier.
  if (kind === "bucket_secret") {
    return (
      <BucketSecretFields
        lockName={lockIdentity}
        bucketSecret={bucketSecret}
        setBucketSecret={setBucketSecret}
      />
    );
  }
  return (
    <>
      <ComposerInput
        label="Name"
        disabled={lockIdentity}
        value={value.keyName}
        placeholder="Variable name"
        onChange={(keyName) => setValue((prev) => ({ ...prev, keyName }))}
      />
      <ComposerInput
        label="Value"
        value={value.value}
        placeholder="Stored value"
        onChange={(nextValue) =>
          setValue((prev) => ({ ...prev, value: nextValue }))
        }
      />
    </>
  );
}

/** Renders the name and write-only value inputs for a generic bucket secret. */
function BucketSecretFields({
  lockName = false,
  bucketSecret,
  setBucketSecret,
}: {
  lockName?: boolean;
  bucketSecret: typeof emptyBucketSecret;
  setBucketSecret: (
    updater: (prev: typeof emptyBucketSecret) => typeof emptyBucketSecret
  ) => void;
}) {
  return (
    <>
      <ComposerInput
        label="Name"
        disabled={lockName}
        value={bucketSecret.keyName}
        placeholder="Secret name"
        onChange={(keyName) =>
          setBucketSecret((prev) => ({ ...prev, keyName }))
        }
      />
      <ComposerInput
        label="Value"
        type="password" data-fused-visible="false"
        value={bucketSecret.value}
        placeholder="Secret value"
        onChange={(nextValue) =>
          setBucketSecret((prev) => ({ ...prev, value: nextValue }))
        }
      />
    </>
  );
}

type SecretStringField = "username" | "password" | "certificate" | "privateKey" | "clientId" | "clientSecret";

/** Renders a non-secret identifier field paired with a masked secret input for two-part credentials. */
function PairedSecretFields({
  secret,
  setSecret,
  primaryLabel,
  primaryPlaceholder,
  primaryField,
  secondaryLabel,
  secondaryPlaceholder,
  secondaryField,
}: {
  secret: typeof emptySecret;
  setSecret: (updater: (prev: typeof emptySecret) => typeof emptySecret) => void;
  primaryLabel: string;
  primaryPlaceholder: string;
  primaryField: SecretStringField;
  secondaryLabel: string;
  secondaryPlaceholder: string;
  secondaryField: SecretStringField;
}) {
  return (
    <>
      <ComposerInput
        label={primaryLabel}
        value={secret[primaryField]}
        placeholder={primaryPlaceholder}
        onChange={(next) => setSecret((prev) => ({ ...prev, [primaryField]: next }))}
      />
      <ComposerInput
        label={secondaryLabel}
        type="password"
        data-fused-visible="false"
        value={secret[secondaryField]}
        placeholder={secondaryPlaceholder}
        onChange={(next) => setSecret((prev) => ({ ...prev, [secondaryField]: next }))}
      />
    </>
  );
}

/** Selects the credential input fields matching the active service auth scheme. */
function SecretValueFields({
  authOption,
  secret,
  setSecret,
}: {
  authOption?: ServiceAuthOption;
  secret: typeof emptySecret;
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void;
}) {
  // Nothing has been identified yet (no service picked, or the picked service
  // has no auth options): there's no way to know which fields to collect, so
  // render nothing instead of a token box no one can fill in meaningfully.
  if (!authOption) return null;
  // HTTP basic auth requires separate username and password inputs.
  if (authOption.required_fields.includes("username")) {
    return (
      <PairedSecretFields
        secret={secret}
        setSecret={setSecret}
        primaryLabel="Username"
        primaryPlaceholder="Username"
        primaryField="username"
        secondaryLabel="Password"
        secondaryPlaceholder="Password"
        secondaryField="password"
      />
    );
  }
  // mTLS credentials pair a client certificate PEM with its private key PEM.
  if (authOption.required_fields.includes("certificate")) {
    return (
      <PairedSecretFields
        secret={secret}
        setSecret={setSecret}
        primaryLabel="Certificate"
        primaryPlaceholder="Client certificate PEM"
        primaryField="certificate"
        secondaryLabel="Private key"
        secondaryPlaceholder="Client private key PEM"
        secondaryField="privateKey"
      />
    );
  }
  // OAuth/OIDC store an application client pair rather than a single opaque
  // token, so they get dedicated Client ID / Client secret inputs.
  if (authOption.required_fields.includes("client_id")) {
    return (
      <PairedSecretFields
        secret={secret}
        setSecret={setSecret}
        primaryLabel="Client ID"
        primaryPlaceholder="Client ID"
        primaryField="clientId"
        secondaryLabel="Client secret"
        secondaryPlaceholder="Client secret"
        secondaryField="clientSecret"
      />
    );
  }
  return (
    <ComposerInput
      label={authOption.auth_type === "api_key" ? "Secret value" : "Token"}
      type="password" data-fused-visible="false"
      value={secret.value}
      placeholder={tokenPlaceholder(authOption.auth_type)}
      onChange={(next) => setSecret((prev) => ({ ...prev, value: next }))}
    />
  );
}

/** Uses the shared select while keeping selection state and actions owned by this page. */
function QualifierSelect({
  kind,
  authOptions,
  secret,
  value,
  disabled = false,
  setSecret,
  setValue,
}: {
  kind: BucketEntryKind;
  authOptions: ServiceAuthOption[];
  secret: typeof emptySecret;
  value: typeof emptyValue;
  disabled?: boolean;
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void;
  setValue: (updater: (prev: typeof emptyValue) => typeof emptyValue) => void;
}) {
  return (
    <label className="min-w-0">
      <span className="mb-1 block text-xs font-medium text-slate-500">
        {kind === "secret" ? "Credential type" : "Location"}
      </span>
      <Select
        value={kind === "secret" ? secret.credentialType : value.location}
        disabled={disabled || (kind === "secret" && authOptions.length === 0)}
        onChange={(event) =>
          updateQualifier(kind, event.target.value, setSecret, setValue)
        }
        className="h-[38px] w-full rounded-lg border border-slate-200 bg-white px-3 text-sm outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-500/20 disabled:bg-slate-50 disabled:text-slate-500"
      >
        {kind === "secret" ? (
          <>
            {authOptions.length === 0 && (
              <option value="">No auth methods</option>
            )}
            {authOptions.map((option) => (
              <option key={option.id} value={option.id}>
                {option.label}
              </option>
            ))}
          </>
        ) : (
          <>
            <option value="header">Header</option>
            <option value="query">Query</option>
            <option value="path">Path</option>
            <option value="body">Body</option>
            <option value="env">Env</option>
          </>
        )}
      </Select>
    </label>
  );
}

/** All composer text fields are required by the credential/value validator; expiry remains separate and optional. */
function ComposerInput({
  label,
  value,
  placeholder,
  onChange,
  disabled = false,
  type = "text",
}: {
  label: string;
  value: string;
  placeholder: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  type?: string;
}) {
  return (
    <label className="min-w-0">
      <span className="mb-1 block text-xs font-medium text-slate-500">
        <FieldLabel required={!disabled}>{label}</FieldLabel>
      </span>
      <input
        type={type}
        required
        disabled={disabled}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        className="h-[38px] w-full rounded-lg border border-slate-200 px-3 text-sm outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-500/20 disabled:bg-slate-50 disabled:text-slate-500"
      />
    </label>
  );
}

// Expiry is optional for every secret kind: an empty value means the stored
// credential never expires, matching the CLI's optional --expires-at flag.
function ExpiryInput({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}) {
  return (
    <label className="min-w-0">
      <span className="mb-1 block text-xs font-medium text-slate-500">
        Expires at
      </span>
      <input
        type="datetime-local"
        value={value}
        onChange={(event) => onChange(event.target.value)}
        className="h-[38px] w-full rounded-lg border border-slate-200 px-3 text-sm outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-500/20"
      />
    </label>
  );
}

function serviceLabel(service: ActivatedService): string {
  const name = service.service_name || "Unnamed service";
  return service.version ? `${name} (${service.version})` : name;
}

async function submitSecret(
  form: typeof emptySecret,
  authOption: ServiceAuthOption | undefined,
  onSave: (payload: SecretFormPayload) => Promise<void>,
  onSaveMany: (payloads: SecretFormPayload[]) => Promise<void>
) {
  if (!authOption) return;
  const payloads = serializeSecretPayloads(form, authOption);
  if (payloads.length === 1) await onSave(payloads[0]);
  else await onSaveMany(payloads);
}

async function submitValue(
  form: typeof emptyValue,
  onSave: (payload: ValueFormPayload) => Promise<void>
) {
  await onSave(serializeValuePayload(form));
}

function resetForms(
  setSecret: (value: typeof emptySecret) => void,
  setValue: (value: typeof emptyValue) => void,
  setBucketSecret: (value: typeof emptyBucketSecret) => void,
  setServiceSearch: (search: string) => void,
  setError: (error: string) => void
) {
  setSecret(emptySecret);
  setValue(emptyValue);
  setBucketSecret(emptyBucketSecret);
  setServiceSearch("");
  setError("");
}

function applyDefaultService(
  kind: BucketEntryKind,
  services: ActivatedService[],
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void,
  setValue: (updater: (prev: typeof emptyValue) => typeof emptyValue) => void,
  setServiceSearch: (search: string) => void
) {
  // Bucket secrets aren't service-scoped, so there is no default service to
  // preselect for them.
  if (kind === "bucket_secret") return;
  if (services.length === 0) return;
  const service = services[0];
  // The form submits service_id because bucket entries are service-scoped, but
  // the user selects a named service so they never have to handle raw UUIDs.
  if (kind === "secret")
    setSecret((prev) =>
      prev.serviceId
        ? prev
        : {
            ...prev,
            serviceId: service.service_id,
            credentialType: firstAuthOptionID(service),
          }
    );
  else
    setValue((prev) =>
      prev.serviceId ? prev : { ...prev, serviceId: service.service_id }
    );
  // The selected ID already drives the button label. Keeping the label out of
  // search means reopening the select still shows every available service.
  setServiceSearch("");
}

function updateServiceID(
  kind: BucketEntryKind,
  serviceId: string,
  services: ActivatedService[],
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void,
  setValue: (updater: (prev: typeof emptyValue) => typeof emptyValue) => void
) {
  if (kind === "secret")
    setSecret((prev) => ({
      ...prev,
      serviceId,
      credentialType: firstAuthOptionID(
        services.find((service) => service.service_id === serviceId)
      ),
    }));
  else setValue((prev) => ({ ...prev, serviceId }));
}

function updateQualifier(
  kind: BucketEntryKind,
  qualifier: string,
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void,
  setValue: (updater: (prev: typeof emptyValue) => typeof emptyValue) => void
) {
  if (kind === "secret")
    setSecret((prev) => ({ ...prev, credentialType: qualifier }));
  else setValue((prev) => ({ ...prev, location: qualifier }));
}

function serviceAuthOptions(
  service: ActivatedService | undefined
): ServiceAuthOption[] {
  // Missing Registry metadata must disable secret creation instead of silently
  // offering credential types that may not exist for the selected service.
  return service?.auth_options ?? [];
}

function firstAuthOptionID(service: ActivatedService | undefined): string {
  return serviceAuthOptions(service)[0]?.id || "";
}

async function saveEntry(
  kind: BucketEntryKind,
  secret: typeof emptySecret,
  value: typeof emptyValue,
  bucketSecret: typeof emptyBucketSecret,
  authOption: ServiceAuthOption | undefined,
  onSaveSecret: (payload: SecretFormPayload) => Promise<void>,
  onSaveSecrets: (payloads: SecretFormPayload[]) => Promise<void>,
  onSaveCredentialFamily: (
    payload: CredentialFamilyFormPayload
  ) => Promise<void>,
  onSaveValue: (payload: ValueFormPayload) => Promise<void>,
  onSaveBucketSecret: (payload: BucketSecretFormPayload) => Promise<void>
) {
  if (kind === "secret") {
    // OAuth/OIDC application pairs use Engine's credential_family write path;
    // every other credential type keeps the existing row-oriented path.
    if (authOption && secretIsCredentialFamily(authOption.credential_type)) {
      await onSaveCredentialFamily(
        serializeCredentialFamilyPayload(secret, authOption)
      );
      return;
    }
    await submitSecret(secret, authOption, onSaveSecret, onSaveSecrets);
    return;
  }
  if (kind === "bucket_secret") {
    await onSaveBucketSecret(serializeBucketSecretPayload(bucketSecret));
    return;
  }
  await submitValue(value, onSaveValue);
}
