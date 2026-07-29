import { type FormEvent, useEffect, useState } from "react";
import { Check, X } from "lucide-react";
import { type ActivatedService } from "~/lib/api";
import type { ServiceAuthOption } from "~/lib/service-auth";
import { BucketServiceSelect } from "~/components/buckets/BucketServiceSelect";
import {
  type BucketEntryKind,
  type SecretFormPayload,
  type ValueFormPayload,
} from "~/lib/buckets";
import {
  secretHasTwoFields,
  serializeSecretPayloads,
  serializeValuePayload,
  type SecretEntryForm,
  tokenPlaceholder,
  validateEntry,
  type ValueEntryForm,
} from "~/components/buckets/BucketEntryComposerModel";

type BucketEntryComposerProps = {
  kind: BucketEntryKind | null;
  saving: boolean;
  services: ActivatedService[];
  onCancel: () => void;
  onSaveSecret: (payload: SecretFormPayload) => Promise<void>;
  onSaveSecrets: (payloads: SecretFormPayload[]) => Promise<void>;
  onSaveValue: (payload: ValueFormPayload) => Promise<void>;
};

const emptySecret: SecretEntryForm = {
  serviceId: "",
  credentialType: "",
  value: "",
  username: "",
  password: "",
  certificate: "",
  privateKey: "",
  expiresAt: "",
};

const emptyValue: ValueEntryForm = {
  serviceId: "",
  keyName: "",
  location: "header",
  value: "",
};

export function BucketEntryComposer({
  kind,
  saving,
  services,
  onCancel,
  onSaveSecret,
  onSaveSecrets,
  onSaveValue,
}: BucketEntryComposerProps) {
  const [secret, setSecret] = useState(emptySecret);
  const [value, setValue] = useState(emptyValue);
  const [serviceSearch, setServiceSearch] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    if (!kind) resetForms(setSecret, setValue, setServiceSearch, setError);
  }, [kind]);

  useEffect(() => {
    if (kind)
      applyDefaultService(
        kind,
        services,
        setSecret,
        setValue,
        setServiceSearch
      );
  }, [kind, services]);

  if (!kind) return null;

  const selectedService = services.find(
    (service) =>
      service.service_id ===
      (kind === "secret" ? secret.serviceId : value.serviceId)
  );
  const authOptions =
    kind === "secret" ? serviceAuthOptions(selectedService) : [];
  const selectedAuthOption = authOptions.find(
    (option) => option.id === secret.credentialType
  );

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const validationError = validateEntry(
      kind,
      secret,
      value,
      selectedAuthOption
    );
    if (validationError) {
      setError(validationError);
      return;
    }
    try {
      await saveEntry(
        kind,
        secret,
        value,
        selectedAuthOption,
        onSaveSecret,
        onSaveSecrets,
        onSaveValue
      );
      onCancel();
    } catch {
      // Route-level save helpers own the toast so the composer can stay focused
      // on form state while still keeping failed submissions open.
    }
  };

  return (
    <form onSubmit={submit} className="border-b border-slate-100 px-6 py-4">
      <div
        className={composerGridClass(kind, selectedAuthOption?.auth_type || "")}
      >
        <ComposerServiceSelect
          services={services}
          search={serviceSearch}
          value={kind === "secret" ? secret.serviceId : value.serviceId}
          onSearchChange={setServiceSearch}
          onChange={(next) => {
            setError("");
            updateServiceID(kind, next, services, setSecret, setValue);
          }}
        />
        <QualifierSelect
          kind={kind}
          authOptions={authOptions}
          secret={secret}
          value={value}
          setSecret={setSecret}
          setValue={setValue}
        />
        <EntryValueFields
          kind={kind}
          authOption={selectedAuthOption}
          secret={secret}
          value={value}
          setSecret={setSecret}
          setValue={setValue}
        />
        <button
          type="submit"
          disabled={saving || services.length === 0}
          className="inline-flex h-[38px] w-[32px] items-center justify-center text-blue-600 hover:text-blue-700 disabled:opacity-40"
          aria-label={`Save ${kind}`}
          title="Save"
        >
          <Check className="w-4 h-4" />
        </button>
        <button
          type="button"
          onClick={onCancel}
          disabled={saving}
          className="inline-flex h-[38px] w-[32px] items-center justify-center text-slate-500 hover:text-slate-700 disabled:opacity-40"
          aria-label="Cancel entry"
          title="Cancel"
        >
          <X className="w-4 h-4" />
        </button>
      </div>
      {error && (
        <p className="mt-2 text-xs font-medium text-red-600">{error}</p>
      )}
    </form>
  );
}

function composerGridClass(
  kind: BucketEntryKind,
  credentialType: string
): string {
  if (kind === "value" || secretHasTwoFields(credentialType)) {
    return "grid grid-cols-1 items-end gap-2 lg:grid-cols-[minmax(220px,0.9fr)_150px_minmax(180px,0.8fr)_minmax(180px,0.8fr)_auto_auto]";
  }
  return "grid grid-cols-1 items-end gap-2 lg:grid-cols-[minmax(220px,0.9fr)_150px_minmax(260px,1fr)_auto_auto]";
}

function ComposerServiceSelect({
  services,
  search,
  value,
  onSearchChange,
  onChange,
}: {
  services: ActivatedService[];
  search: string;
  value: string;
  onSearchChange: (search: string) => void;
  onChange: (value: string) => void;
}) {
  return (
    <div className="min-w-0">
      <BucketServiceSelect
        id="bucket-entry-service"
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

function EntryValueFields({
  kind,
  authOption,
  secret,
  value,
  setSecret,
  setValue,
}: {
  kind: BucketEntryKind;
  authOption?: ServiceAuthOption;
  secret: typeof emptySecret;
  value: typeof emptyValue;
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void;
  setValue: (updater: (prev: typeof emptyValue) => typeof emptyValue) => void;
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
  return (
    <>
      <ComposerInput
        label="Name"
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
  if (authOption?.required_fields.includes("username")) {
    return <BasicSecretFields secret={secret} setSecret={setSecret} />;
  }
  if (authOption?.required_fields.includes("certificate")) {
    return <MTLSSecretFields secret={secret} setSecret={setSecret} />;
  }
  return (
    <ComposerInput
      label={authOption?.auth_type === "api_key" ? "Secret value" : "Token"}
      type="password"
      value={secret.value}
      placeholder={
        authOption
          ? tokenPlaceholder(authOption.auth_type)
          : "Choose a credential type"
      }
      onChange={(next) => setSecret((prev) => ({ ...prev, value: next }))}
    />
  );
}

function BasicSecretFields({
  secret,
  setSecret,
}: {
  secret: typeof emptySecret;
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void;
}) {
  return (
    <>
      <ComposerInput
        label="Username"
        value={secret.username}
        placeholder="Username"
        onChange={(username) => setSecret((prev) => ({ ...prev, username }))}
      />
      <ComposerInput
        label="Password"
        type="password"
        value={secret.password}
        placeholder="Password"
        onChange={(password) => setSecret((prev) => ({ ...prev, password }))}
      />
    </>
  );
}

function MTLSSecretFields({
  secret,
  setSecret,
}: {
  secret: typeof emptySecret;
  setSecret: (
    updater: (prev: typeof emptySecret) => typeof emptySecret
  ) => void;
}) {
  return (
    <>
      <ComposerInput
        label="Certificate"
        value={secret.certificate}
        placeholder="Client certificate PEM"
        onChange={(certificate) =>
          setSecret((prev) => ({ ...prev, certificate }))
        }
      />
      <ComposerInput
        label="Private key"
        type="password"
        value={secret.privateKey}
        placeholder="Client private key PEM"
        onChange={(privateKey) =>
          setSecret((prev) => ({ ...prev, privateKey }))
        }
      />
    </>
  );
}

function QualifierSelect({
  kind,
  authOptions,
  secret,
  value,
  setSecret,
  setValue,
}: {
  kind: BucketEntryKind;
  authOptions: ServiceAuthOption[];
  secret: typeof emptySecret;
  value: typeof emptyValue;
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
      <select
        value={kind === "secret" ? secret.credentialType : value.location}
        disabled={kind === "secret" && authOptions.length === 0}
        onChange={(event) =>
          updateQualifier(kind, event.target.value, setSecret, setValue)
        }
        className="h-[38px] w-full rounded-lg border border-slate-200 bg-white px-3 text-sm outline-none focus:border-blue-500 focus:ring-2 focus:ring-blue-500/20"
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
            <option value="body">Body</option>
          </>
        )}
      </select>
    </label>
  );
}

function ComposerInput({
  label,
  value,
  placeholder,
  onChange,
  type = "text",
}: {
  label: string;
  value: string;
  placeholder: string;
  onChange: (value: string) => void;
  type?: string;
}) {
  return (
    <label className="min-w-0">
      <span className="mb-1 block text-xs font-medium text-slate-500">
        {label}
      </span>
      <input
        type={type}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
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
  setServiceSearch: (search: string) => void,
  setError: (error: string) => void
) {
  setSecret(emptySecret);
  setValue(emptyValue);
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
  authOption: ServiceAuthOption | undefined,
  onSaveSecret: (payload: SecretFormPayload) => Promise<void>,
  onSaveSecrets: (payloads: SecretFormPayload[]) => Promise<void>,
  onSaveValue: (payload: ValueFormPayload) => Promise<void>
) {
  if (kind === "secret")
    await submitSecret(secret, authOption, onSaveSecret, onSaveSecrets);
  else await submitValue(value, onSaveValue);
}
