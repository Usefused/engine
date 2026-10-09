import { FieldLabel } from "~/components/forms/FieldLabel";
import { useEffect, useState } from "react";
import { HostedConnectionPreview } from "./HostedConnectionPreview";

import { useToast } from "~/components/Toast";
import { SettingsDisclosureCard } from "~/components/settings/SettingsDisclosureCard";
import { api, type ConnectBrandingInput } from "~/lib/api";
import {
  connectBrandingConfirmationSummary,
  connectBrandingInput,
  connectBrandingPreviewName,
  emptyConnectBrandingInput,
  normalizeConnectBrandingInput,
  safeLogoPreviewURL,
  safePrimaryColour,
  validateConnectBrandingInput,
  type ConnectBrandingErrors,
  type ConnectBrandingField,
  type ConnectBrandingConfirmationSummary,
} from "~/lib/connect-branding";

const INPUT_CLASS =
  "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 transition-shadow focus:border-transparent focus:outline-none focus:ring-2 focus:ring-blue-500";

// errorMessage preserves actionable authorization text produced by the shared API client.
function errorMessage(error: unknown, fallback: string): string {
  // Typed request errors already contain product-language authorization guidance.
  if (error instanceof Error) return error.message;
  return fallback;
}

// ConnectBrandingCard edits and previews the Engine-owned appearance of hosted connection pages.
export function ConnectBrandingCard({ defaultExpanded = false }: { defaultExpanded?: boolean }) {
  const toast = useToast();
  const [draft, setDraft] = useState<ConnectBrandingInput>(
    emptyConnectBrandingInput,
  );
  const [persisted, setPersisted] = useState<ConnectBrandingInput>(
    emptyConnectBrandingInput,
  );
  const [pendingSave, setPendingSave] = useState<ConnectBrandingInput | null>(
    null,
  );
  const [errors, setErrors] = useState<ConnectBrandingErrors>({});
  const [loadError, setLoadError] = useState("");
  const [saveError, setSaveError] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  // The branding request is independent from account settings so either card can remain usable if the other fails.
  useEffect(() => {
    let active = true;

    // loadBranding applies a response only while this card remains mounted.
    async function loadBranding() {
      try {
        const branding = await api.connectBranding.get();
        // Mounted cards alone may accept an asynchronous response.
        if (active) {
          const input = connectBrandingInput(branding);
          setDraft(input);
          setPersisted(input);
        }
      } catch (error) {
        // Navigation must not resurrect an error inside an unmounted card.
        if (active) {
          setLoadError(
            errorMessage(error, "Failed to load connection branding."),
          );
        }
      } finally {
        // Loading state belongs to the current card instance only.
        if (active) setLoading(false);
      }
    }

    void loadBranding();
    // Stale requests must not replace settings after navigation.
    return () => {
      active = false;
    };
  }, []);

  const previewLogoURL = safeLogoPreviewURL(draft.logo_url);
  const previewName = connectBrandingPreviewName(draft.display_name);
  const previewColour = safePrimaryColour(draft.primary_color);
  // Confirmation is derived from the immutable pending snapshot so later state cannot alter the reviewed change.
  const pendingSummary = pendingSave
    ? connectBrandingConfirmationSummary(persisted, pendingSave)
    : null;

  // updateField centralizes clearing stale validation and success state for edits.
  function updateField(field: ConnectBrandingField, value: string) {
    // Once PUT begins, the reviewed snapshot remains authoritative until Engine responds.
    if (saving) return;
    // Immutable field replacement keeps React state updates predictable across rapid input events.
    setDraft((current) => ({ ...current, [field]: value }));
    // Editing one field clears only its stale validation message.
    setErrors((current) => ({ ...current, [field]: undefined }));
    setSaveError("");
    setSaved(false);
    setPendingSave(null);
  }

  // handleFieldChange routes every named branding control through one typed draft updater.
  function handleFieldChange(event: React.ChangeEvent<HTMLInputElement>) {
    updateField(event.currentTarget.name as ConnectBrandingField, event.currentTarget.value);
  }

  // handleSubmit validates locally and opens confirmation without contacting Engine.
  function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    // An open confirmation or active write owns the current submit cycle.
    if (pendingSave || saving) return;
    const normalized = normalizeConnectBrandingInput(draft);
    const validationErrors = validateConnectBrandingInput(normalized);
    // Invalid drafts remain local so Engine receives only values matching its contract.
    if (Object.keys(validationErrors).length > 0) {
      setErrors(validationErrors);
      setSaved(false);
      return;
    }

    setSaveError("");
    setSaved(false);
    setPendingSave(normalized);
  }

  // handleConfirmSave persists only the exact snapshot reviewed in the confirmation prompt.
  async function handleConfirmSave() {
    // Native button disabling is reinforced here so rapid programmatic activation cannot duplicate PUT requests.
    if (!pendingSave || saving) return;
    setSaving(true);
    setSaveError("");
    try {
      const branding = await api.connectBranding.update(pendingSave);
      const savedInput = connectBrandingInput(branding);
      setDraft(savedInput);
      setPersisted(savedInput);
      setPendingSave(null);
      setSaved(true);
      toast.success("Connection branding saved.");
    } catch (error) {
      // Engine-authored errors retain permission and remediation details for the operator.
      const message = errorMessage(
        error,
        "Failed to save connection branding.",
      );
      setSaveError(message);
      toast.error(message);
    } finally {
      setSaving(false);
    }
  }

  // handleCancelSave closes confirmation while leaving every draft field untouched.
  function handleCancelSave() {
    // An in-flight write cannot be cancelled after Engine has received it.
    if (saving) return;
    setPendingSave(null);
    setSaveError("");
  }

  // Loading content uses a stable skeleton until initialized values can safely populate the form.
  const content = loading ? (
    <BrandingLoading />
  ) : (
    <ConnectBrandingEditor
      draft={draft}
      errors={errors}
      loadError={loadError}
      saveError={saveError}
      saving={saving}
      saved={saved}
      previewName={previewName}
      previewColour={previewColour}
      previewLogoURL={previewLogoURL}
      pendingSummary={pendingSummary}
      onSubmit={handleSubmit}
      onConfirmSave={handleConfirmSave}
      onCancelSave={handleCancelSave}
      onFieldChange={handleFieldChange}
    />
  );
  return (
    <SettingsDisclosureCard
      defaultExpanded={defaultExpanded}
      id="connect-branding-settings"
      title="Connect branding"
      description="Customize Fused-hosted pages customers see before and after an OAuth provider handoff."
    >
      {content}
    </SettingsDisclosureCard>
  );
}

// BrandingLoading prevents incomplete defaults from flashing while preserving the surrounding disclosure state.
function BrandingLoading() {
  return (
    <div className="animate-pulse space-y-4" aria-label="Loading connection branding">
      <div className="h-10 w-full rounded bg-slate-100" />
      <div className="h-32 w-full rounded bg-slate-100" />
    </div>
  );
}

interface ConnectBrandingEditorProps {
  draft: ConnectBrandingInput;
  errors: ConnectBrandingErrors;
  loadError: string;
  saveError: string;
  saving: boolean;
  saved: boolean;
  previewName: string;
  previewColour: string;
  previewLogoURL: string | null;
  pendingSummary: ConnectBrandingConfirmationSummary | null;
  onSubmit: (event: React.FormEvent<HTMLFormElement>) => void;
  onConfirmSave: () => void;
  onCancelSave: () => void;
  onFieldChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
}

// ConnectBrandingEditor uses the full section width to keep compact editing controls beside the hosted preview.
function ConnectBrandingEditor(props: ConnectBrandingEditorProps) {
  return (
    <>
      <InlineError message={props.loadError} extraClass="mb-5" />
      <form className="space-y-5" onSubmit={props.onSubmit} noValidate toolname="save_connect_branding" tooldescription="Save the branding displayed on hosted connection pages.">
        <div className="grid items-start gap-8 xl:grid-cols-2">
          <div className="min-w-0 space-y-5">
            <BrandingFields
              draft={props.draft}
              errors={props.errors}
              saving={props.saving}
              previewColour={props.previewColour}
              onFieldChange={props.onFieldChange}
            />
            <InlineError message={props.saveError} />
            <SaveControls saving={props.saving} saved={props.saved} disabled={Boolean(props.loadError) || Boolean(props.pendingSummary)} />
          </div>
          <HostedConnectionPreview
            previewName={props.previewName}
            previewColour={props.previewColour}
            previewLogoURL={props.previewLogoURL}
            supportURL={props.draft.support_url}
            privacyURL={props.draft.privacy_url}
          />
        </div>
      </form>
      <BrandingConfirmation
        summary={props.pendingSummary}
        saving={props.saving}
        onConfirm={props.onConfirmSave}
        onCancel={props.onCancelSave}
      />
    </>
  );
}

interface BrandingConfirmationProps {
  summary: ConnectBrandingConfirmationSummary | null;
  saving: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

// BrandingConfirmation requests explicit consent using facts that never reveal names, colours, or full URLs.
function BrandingConfirmation({
  summary,
  saving,
  onConfirm,
  onCancel,
}: BrandingConfirmationProps) {
  // The prompt remains absent until a valid draft has been submitted for review.
  if (!summary) return null;
  // Progress text distinguishes an accepted write from a prompt awaiting a decision.
  const confirmLabel = saving ? "Saving..." : "Confirm save";
  return (
    <div
      role="alertdialog"
      aria-labelledby="connect-branding-confirm-title"
      aria-describedby="connect-branding-confirm-description"
      className="mt-5 rounded-lg border border-blue-200 bg-blue-50 p-4"
    >
      <h3 id="connect-branding-confirm-title" className="text-sm font-semibold text-slate-900">
        Confirm branding changes
      </h3>
      <p id="connect-branding-confirm-description" className="mt-1 text-sm text-slate-600">
        Review which fields will change and whether external assets or links are configured before updating Fused-hosted connection pages.
      </p>
      <dl className="mt-4 grid gap-2 text-sm sm:grid-cols-2">
        <SummaryFact label="App name" value={changeLabel(summary.displayNameChanged)} />
        <SummaryFact label="Primary colour" value={changeLabel(summary.primaryColorChanged)} />
        <SummaryFact label="External logo" value={assetLabel(summary.logoChanged, summary.logoPresent)} />
        <SummaryFact label="Support link" value={assetLabel(summary.supportURLChanged, summary.supportURLPresent)} />
        <SummaryFact label="Privacy link" value={assetLabel(summary.privacyURLChanged, summary.privacyURLPresent)} />
      </dl>
      <div className="mt-4 flex gap-3">
        <button
          type="button"
          autoFocus
          disabled={saving}
          onClick={onConfirm}
          className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-50"
        >
          {confirmLabel}
        </button>
        <button
          type="button"
          disabled={saving}
          onClick={onCancel}
          className="rounded-lg border border-slate-300 bg-white px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50"
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

// changeLabel translates one comparison flag without exposing either field value.
function changeLabel(changed: boolean): string {
  // Changed and unchanged states are intentionally the only possible disclosure.
  return changed ? "Will change" : "Unchanged";
}

// assetLabel combines change and presence facts without retaining or rendering the underlying URL.
function assetLabel(changed: boolean, present: boolean): string {
  const change = changeLabel(changed);
  // Presence says whether the saved page will render a link or image, never where it points.
  const presence = present ? "configured" : "not configured";
  return `${change}; ${presence}`;
}

// SummaryFact keeps every safe confirmation fact visually consistent.
function SummaryFact({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md bg-white px-3 py-2">
      <dt className="font-medium text-slate-700">{label}</dt>
      <dd className="text-slate-500">{value}</dd>
    </div>
  );
}

interface BrandingFieldsProps {
  draft: ConnectBrandingInput;
  errors: ConnectBrandingErrors;
  saving: boolean;
  previewColour: string;
  onFieldChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
}

// BrandingFields pairs short fields and related URLs while leaving the logo address a full row.
function BrandingFields(props: BrandingFieldsProps) {
  return (
    <fieldset disabled={props.saving} className="grid min-w-0 gap-4 sm:grid-cols-2">
      <div>
        <label htmlFor="connect-display-name" className="mb-1 block text-sm font-medium text-slate-700"><FieldLabel required>App name</FieldLabel></label>
        <input id="connect-display-name" name="display_name" type="text" required value={props.draft.display_name} onChange={props.onFieldChange} className={INPUT_CLASS} aria-invalid={Boolean(props.errors.display_name)} />
        <FieldError message={props.errors.display_name} />
      </div>
      <div>
        <label htmlFor="connect-primary-color" className="mb-1 block text-sm font-medium text-slate-700"><FieldLabel required>Primary colour</FieldLabel></label>
        <div className="flex h-[38px] items-center gap-2 rounded-lg border border-slate-300 bg-white px-2 focus-within:ring-2 focus-within:ring-blue-500">
          <input id="connect-primary-color-picker" name="primary_color" type="color" value={props.previewColour} onChange={props.onFieldChange} className="h-7 w-7 shrink-0 cursor-pointer border-0 bg-transparent p-0" aria-label="Choose primary colour" />
          <input id="connect-primary-color" name="primary_color" type="text" maxLength={7} required value={props.draft.primary_color} onChange={props.onFieldChange} className="min-w-0 flex-1 border-0 bg-transparent p-0 text-sm text-slate-900 focus:outline-none focus:ring-0" aria-invalid={Boolean(props.errors.primary_color)} />
        </div>
        <FieldError message={props.errors.primary_color} />
      </div>
      <div className="sm:col-span-2">
        <label htmlFor="connect-logo-url" className="mb-1 block text-sm font-medium text-slate-700">Logo URL <span className="font-normal text-slate-400">(optional)</span></label>
        <input id="connect-logo-url" name="logo_url" type="url" inputMode="url" maxLength={2048} placeholder="https://assets.example.com/logo.png" value={props.draft.logo_url} onChange={props.onFieldChange} className={INPUT_CLASS} aria-invalid={Boolean(props.errors.logo_url)} />
        <p className="mt-1 text-xs text-slate-500">Use an absolute HTTPS image URL. Leave blank to show the app name without a logo.</p>
        <FieldError message={props.errors.logo_url} />
      </div>
      <URLField id="connect-support-url" name="support_url" label="Support URL" value={props.draft.support_url} error={props.errors.support_url} onChange={props.onFieldChange} />
      <URLField id="connect-privacy-url" name="privacy_url" label="Privacy-policy URL" value={props.draft.privacy_url} error={props.errors.privacy_url} onChange={props.onFieldChange} />
    </fieldset>
  );
}

// InlineError keeps request failures visible without reserving empty space in the form.
function InlineError({ message, extraClass = "" }: { message: string; extraClass?: string }) {
  // Empty errors render nothing so the form does not reserve misleading alert space.
  if (!message) return null;
  // Present errors use an alert role so assistive technology announces request failures.
  return <p role="alert" className={`${extraClass} rounded-lg bg-red-50 p-3 text-sm text-red-700`}>{message}</p>;
}

// SaveControls reports progress and success independently from validation errors.
function SaveControls({ saving, saved, disabled }: { saving: boolean; saved: boolean; disabled: boolean }) {
  // Loading failures and in-flight writes both prevent duplicate or stale updates.
  const saveDisabled = saving || disabled;
  // Progress text makes the active mutation explicit without changing button geometry.
  const saveLabel = saving ? "Saving..." : "Save branding";
  return (
    <div className="flex items-center gap-4 pt-1">
      <button type="submit" data-track="save_connect_branding" disabled={saveDisabled} className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-slate-800 focus:outline-none focus:ring-2 focus:ring-slate-950 focus:ring-offset-2 disabled:opacity-50">
        {saveLabel}
      </button>
      <SavedStatus saved={saved} />
    </div>
  );
}

// SavedStatus announces confirmation only after a completed Engine response.
function SavedStatus({ saved }: { saved: boolean }) {
  // Unsaved and edited drafts should not retain an obsolete success message.
  if (!saved) return null;
  // Successful persistence is announced without moving focus away from the form.
  return <span role="status" className="text-sm text-green-600">Saved successfully!</span>;
}

interface URLFieldProps {
  id: string;
  name: "support_url" | "privacy_url";
  label: string;
  value: string;
  error?: string;
  onChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
}

// URLField keeps optional HTTPS link inputs visually and behaviorally consistent.
function URLField({ id, name, label, value, error, onChange }: URLFieldProps) {
  return (
    <div>
      <label htmlFor={id} className="mb-1 block text-sm font-medium text-slate-700">
        {label} <span className="font-normal text-slate-400">(optional)</span>
      </label>
      <input
        id={id}
        name={name}
        type="url"
        inputMode="url"
        maxLength={2048}
        placeholder="https://example.com"
        value={value}
        onChange={onChange}
        className={INPUT_CLASS}
        aria-invalid={Boolean(error)}
      />
      <FieldError message={error} />
    </div>
  );
}

// FieldError gives every invalid control a consistent nearby explanation.
function FieldError({ message }: { message?: string }) {
  // Valid controls remain compact while invalid controls receive adjacent guidance.
  if (!message) return null;
  // Invalid controls receive concise local guidance before another save attempt.
  return <p className="mt-1 text-xs text-red-600">{message}</p>;
}
