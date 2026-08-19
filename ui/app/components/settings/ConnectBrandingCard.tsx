import { useEffect, useState } from "react";

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
export function ConnectBrandingCard() {
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
  const [logoFailed, setLogoFailed] = useState(false);

  // The branding request is independent from account settings so either card can remain usable if the other fails.
  useEffect(() => {
    let active = true;

    // loadBranding applies a response only while this card remains mounted.
    async function loadBranding() {
      setLoading(true);
      setLoadError("");
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

  // A changed URL gets a fresh image-load attempt in the live preview.
  useEffect(() => {
    setLogoFailed(false);
  }, [draft.logo_url]);

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

  // handleDisplayNameChange maps the display-name input onto the typed branding draft.
  function handleDisplayNameChange(event: React.ChangeEvent<HTMLInputElement>) {
    updateField("display_name", event.target.value);
  }

  // handleLogoURLChange maps the logo URL input onto the typed branding draft.
  function handleLogoURLChange(event: React.ChangeEvent<HTMLInputElement>) {
    updateField("logo_url", event.target.value);
  }

  // handlePrimaryColourChange keeps the colour picker and text value synchronized.
  function handlePrimaryColourChange(event: React.ChangeEvent<HTMLInputElement>) {
    updateField("primary_color", event.target.value);
  }

  // handleSupportURLChange maps the optional support link onto the typed branding draft.
  function handleSupportURLChange(event: React.ChangeEvent<HTMLInputElement>) {
    updateField("support_url", event.target.value);
  }

  // handlePrivacyURLChange maps the optional privacy link onto the typed branding draft.
  function handlePrivacyURLChange(event: React.ChangeEvent<HTMLInputElement>) {
    updateField("privacy_url", event.target.value);
  }

  // handleLogoError replaces a failed external image with the deterministic monogram fallback.
  function handleLogoError() {
    setLogoFailed(true);
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
    setSaved(false);
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
      logoFailed={logoFailed}
      pendingSummary={pendingSummary}
      onSubmit={handleSubmit}
      onConfirmSave={handleConfirmSave}
      onCancelSave={handleCancelSave}
      onDisplayNameChange={handleDisplayNameChange}
      onLogoURLChange={handleLogoURLChange}
      onPrimaryColourChange={handlePrimaryColourChange}
      onSupportURLChange={handleSupportURLChange}
      onPrivacyURLChange={handlePrivacyURLChange}
      onLogoError={handleLogoError}
    />
  );
  return (
    <SettingsDisclosureCard
      id="connect-branding-settings"
      title="Connect branding"
      description="Customize the Engine-hosted pages customers see before and after an OAuth provider handoff."
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
  logoFailed: boolean;
  pendingSummary: ConnectBrandingConfirmationSummary | null;
  onSubmit: (event: React.FormEvent<HTMLFormElement>) => void;
  onConfirmSave: () => void;
  onCancelSave: () => void;
  onDisplayNameChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
  onLogoURLChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
  onPrimaryColourChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
  onSupportURLChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
  onPrivacyURLChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
  onLogoError: () => void;
}

// ConnectBrandingEditor renders the loaded form without mixing persistence state transitions into its markup.
function ConnectBrandingEditor(props: ConnectBrandingEditorProps) {
  return (
    <>
      <InlineError message={props.loadError} extraClass="mb-5" />
      <form className="space-y-5" onSubmit={props.onSubmit} noValidate toolname="save_connect_branding" tooldescription="Save the branding displayed on hosted connection pages.">
        <div className="grid gap-6 lg:grid-cols-2">
          <BrandingFields {...props} />
          <BrandingPreview {...props} />
        </div>
        <InlineError message={props.saveError} />
        <SaveControls saving={props.saving} saved={props.saved} disabled={Boolean(props.loadError) || Boolean(props.pendingSummary)} />
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
      aria-modal="false"
      aria-labelledby="connect-branding-confirm-title"
      aria-describedby="connect-branding-confirm-description"
      className="mt-5 rounded-lg border border-blue-200 bg-blue-50 p-4"
    >
      <h3 id="connect-branding-confirm-title" className="text-sm font-semibold text-slate-900">
        Confirm branding changes
      </h3>
      <p id="connect-branding-confirm-description" className="mt-1 text-sm text-slate-600">
        Review which fields will change and whether external assets or links are configured before updating Engine-hosted connection pages.
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
          className="rounded-lg bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-700 disabled:opacity-50"
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

// BrandingFields groups the editable values while reusing the card's typed event handlers.
function BrandingFields(props: ConnectBrandingEditorProps) {
  return (
    <fieldset disabled={props.saving} className="space-y-4">
      <div>
        <label htmlFor="connect-display-name" className="mb-1 block text-sm font-medium text-slate-700">App name</label>
        <input id="connect-display-name" name="display_name" type="text" required value={props.draft.display_name} onChange={props.onDisplayNameChange} className={INPUT_CLASS} aria-invalid={Boolean(props.errors.display_name)} />
        <FieldError message={props.errors.display_name} />
      </div>
      <div>
        <label htmlFor="connect-logo-url" className="mb-1 block text-sm font-medium text-slate-700">Logo URL <span className="font-normal text-slate-400">(optional)</span></label>
        <input id="connect-logo-url" name="logo_url" type="url" inputMode="url" maxLength={2048} placeholder="https://assets.example.com/logo.png" value={props.draft.logo_url} onChange={props.onLogoURLChange} className={INPUT_CLASS} aria-invalid={Boolean(props.errors.logo_url)} />
        <p className="mt-1 text-xs text-slate-500">Use an absolute HTTPS image URL. Leave blank to use the built-in fallback.</p>
        <FieldError message={props.errors.logo_url} />
      </div>
      <div>
        <label htmlFor="connect-primary-color" className="mb-1 block text-sm font-medium text-slate-700">Primary colour</label>
        <div className="flex gap-2">
          <input id="connect-primary-color-picker" type="color" value={props.previewColour} onChange={props.onPrimaryColourChange} className="h-10 w-12 cursor-pointer rounded border border-slate-300 bg-white p-1" aria-label="Choose primary colour" />
          <input id="connect-primary-color" name="primary_color" type="text" maxLength={7} required value={props.draft.primary_color} onChange={props.onPrimaryColourChange} className={INPUT_CLASS} aria-invalid={Boolean(props.errors.primary_color)} />
        </div>
        <FieldError message={props.errors.primary_color} />
      </div>
      <URLField id="connect-support-url" label="Support URL" value={props.draft.support_url} error={props.errors.support_url} onChange={props.onSupportURLChange} />
      <URLField id="connect-privacy-url" label="Privacy-policy URL" value={props.draft.privacy_url} error={props.errors.privacy_url} onChange={props.onPrivacyURLChange} />
    </fieldset>
  );
}

// BrandingPreview confines external content to a validated image and inert text/button-like elements.
function BrandingPreview(props: ConnectBrandingEditorProps) {
  return (
    <div>
      <p className="mb-2 text-sm font-medium text-slate-700">Live preview</p>
      <div className="overflow-hidden rounded-xl border border-slate-200 bg-slate-50">
        <div className="h-2" style={{ backgroundColor: props.previewColour }} />
        <div className="flex min-h-64 flex-col items-center justify-center p-8 text-center">
          <PreviewLogo {...props} />
          <h3 className="text-xl font-semibold text-slate-900">Connect to {props.previewName}</h3>
          <p className="mt-2 text-sm text-slate-500">Provide the details needed to continue securely.</p>
          <span className="mt-5 rounded-lg px-4 py-2 text-sm font-medium text-white" style={{ backgroundColor: props.previewColour }}>Continue</span>
        </div>
      </div>
    </div>
  );
}

// PreviewLogo falls back to a local monogram when the external URL is absent, invalid, or unavailable.
function PreviewLogo(props: ConnectBrandingEditorProps) {
  // A validated, successfully loaded external image takes precedence over the local mark.
  if (props.previewLogoURL && !props.logoFailed) {
    return <img src={props.previewLogoURL} alt={`${props.previewName} logo`} referrerPolicy="no-referrer" onError={props.onLogoError} className="mb-5 max-h-16 max-w-44 object-contain" />;
  }
  // Missing and failed external images share a deterministic same-origin fallback.
  return (
    <div className="mb-5 flex h-14 w-14 items-center justify-center rounded-xl text-xl font-semibold text-white" style={{ backgroundColor: props.previewColour }} aria-label="Logo fallback">
      {props.previewName.slice(0, 1).toUpperCase()}
    </div>
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
      <button type="submit" data-track="save_connect_branding" disabled={saveDisabled} className="rounded-lg bg-blue-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:ring-offset-2 disabled:opacity-50">
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
  label: string;
  value: string;
  error?: string;
  onChange: (event: React.ChangeEvent<HTMLInputElement>) => void;
}

// URLField keeps optional HTTPS link inputs visually and behaviorally consistent.
function URLField({ id, label, value, error, onChange }: URLFieldProps) {
  return (
    <div>
      <label htmlFor={id} className="mb-1 block text-sm font-medium text-slate-700">
        {label} <span className="font-normal text-slate-400">(optional)</span>
      </label>
      <input
        id={id}
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
