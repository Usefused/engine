package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const maxConnectInputFormBytes = 64 << 10

type connectInputPage struct {
	Action     string
	FormOrigin string
	Fields     []connectInputPageField
	Error      string
	Expired    bool
	Branding   hostedConnectBranding
}

type connectInputPageField struct {
	Name          string
	Label         string
	Value         string
	Placeholder   string
	Description   string
	DescriptionID string
	Required      bool
	Select        bool
	Options       []connectInputPageOption
}

type connectInputPageOption struct {
	Value    string
	Label    string
	Selected bool
}

type connectInputProviderPage struct {
	AuthorizeURL string
	Branding     hostedConnectBranding
}

// hostedConnectBranding contains only validated presentation values plus the
// exact origin admitted for an optional externally hosted logo.
type hostedConnectBranding struct {
	store.ConnectBranding
	LogoOrigin string
}

// AccentForeground returns a contrast-safe action label for the validated
// workspace accent without changing the operator's selected colour.
func (b hostedConnectBranding) AccentForeground() string {
	luminance, ok := connectAccentLuminance(b.PrimaryColor)
	// Invalid values can only arise in direct tests because persisted branding
	// passes strict validation before reaching a hosted page.
	if !ok {
		return "#ffffff"
	}
	// White meets WCAG AA only below this relative-luminance boundary; black
	// safely covers every brighter accent without a second ambiguous range.
	if luminance <= 0.1833 {
		return "#ffffff"
	}
	return "#000000"
}

// connectAccentLuminance decodes the closed #RRGGBB contract into WCAG
// relative luminance for action-foreground selection.
func connectAccentLuminance(value string) (float64, bool) {
	if len(value) != 7 || value[0] != '#' {
		return 0, false
	}
	rgb, err := hex.DecodeString(value[1:])
	if err != nil || len(rgb) != 3 {
		return 0, false
	}
	return 0.2126*connectSRGBChannel(rgb[0]) + 0.7152*connectSRGBChannel(rgb[1]) + 0.0722*connectSRGBChannel(rgb[2]), true
}

// connectSRGBChannel linearizes one colour channel using the WCAG transfer
// function rather than treating encoded RGB bytes as perceptually uniform.
func connectSRGBChannel(value byte) float64 {
	channel := float64(value) / 255
	// Low-intensity channels use the linear segment of the sRGB transfer curve.
	if channel <= 0.04045 {
		return channel / 12.92
	}
	return math.Pow((channel+0.055)/1.055, 2.4)
}

type resolvedConnectInputSession struct {
	session  *store.ConnectInputSession
	resolved connectRuntimeConfig
	values   map[string]string
}

const hostedConnectShellTemplate = `{{define "hosted-connect-shell-style"}}<style>
  :root{color-scheme:only light;font-family:Inter,ui-sans-serif,system-ui,sans-serif}
  *{box-sizing:border-box}
  body{min-height:100svh;margin:0;padding:clamp(1rem,4vw,3rem);background:#fbfaf8;color:#15121c}
  main{width:min(100%,29rem);margin:clamp(1rem,8vh,6rem) auto;padding:clamp(1.25rem,4vw,2rem);background:#fff;border:1px solid #e7e2ea;border-top:3px solid var(--connect-accent);border-radius:1rem;box-shadow:0 18px 50px rgba(21,18,28,.09)}
  .connect-brand{display:flex;align-items:center;gap:.7rem;margin-bottom:2rem;font-size:.9rem;font-weight:750;color:#15121c;overflow-wrap:anywhere}
  .connect-logo{width:40px;height:40px;object-fit:contain}
  .connect-eyebrow{margin:0 0 .65rem;font-size:.72rem;line-height:1.2;font-weight:800;letter-spacing:.09em;text-transform:uppercase;color:#4f2bd4}
  .connect-eyebrow[data-tone="success"]{color:#047857}
  .connect-eyebrow[data-tone="warning"]{color:#a16207}
  .connect-eyebrow[data-tone="danger"]{color:#b91c1c}
  h1{font-size:clamp(1.55rem,5vw,2rem);line-height:1.15;letter-spacing:-.025em;margin:0;color:#15121c;overflow-wrap:anywhere}
  .connect-copy{margin:.85rem 0 0;color:rgba(21,18,28,.68);line-height:1.6}
  .connect-action{display:flex;min-height:46px;width:100%;align-items:center;justify-content:center;margin-top:1.5rem;padding:.78rem 1rem;border:1px solid rgba(21,18,28,.18);border-radius:.7rem;background:var(--connect-accent);color:var(--connect-accent-foreground);font:inherit;font-weight:750;text-align:center;text-decoration:none;cursor:pointer;box-shadow:0 1px 2px rgba(21,18,28,.08);transition:filter .15s ease,transform .15s ease}
  .connect-action:hover{filter:brightness(.96)}
  .connect-action:active{transform:translateY(1px)}
  .connect-links{display:flex;flex-wrap:wrap;gap:1rem;margin-top:2rem;padding-top:1.25rem;border-top:1px solid #f0edf2;font-size:.875rem}
  .connect-links a{color:#4f2bd4}
  :focus-visible{outline:3px solid #6941ff;outline-offset:2px}
  @media(max-width:36rem){body{padding:1rem}main{margin:1rem auto;border-radius:.85rem}.connect-brand{margin-bottom:1.5rem}}
</style>{{end}}`

// parseHostedConnectTemplate installs one trusted light shell around every
// hosted state so browser colour preferences cannot change the owner's brand.
func parseHostedConnectTemplate(name, page string) *template.Template {
	return template.Must(template.New(name).Parse(hostedConnectShellTemplate + page))
}

var connectInputTemplate = parseHostedConnectTemplate("connect-input", `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Connect your account · {{.Branding.DisplayName}}</title>
  {{template "hosted-connect-shell-style"}}
  <style>
    .connect-form{margin-top:1.5rem}.connect-form-helper{margin:0 0 1.25rem;color:rgba(21,18,28,.58);font-size:.875rem;line-height:1.5}.connect-field+.connect-field{margin-top:1.15rem}.connect-label{display:flex;align-items:baseline;justify-content:space-between;gap:1rem;margin:0 0 .45rem;font-size:.92rem;font-weight:700;overflow-wrap:anywhere}.connect-field-requirement{flex:none;color:rgba(21,18,28,.52);font-size:.72rem;font-weight:650;text-transform:uppercase;letter-spacing:.055em}.connect-field-description{margin:.4rem 0 0;color:rgba(21,18,28,.58);font-size:.82rem;line-height:1.45;overflow-wrap:anywhere}input,select{width:100%;min-height:46px;padding:.72rem .8rem;border:1px solid #b8b1bc;border-radius:.65rem;background:#fff;color:#15121c;font:inherit;font-size:1rem;box-shadow:0 1px 2px rgba(21,18,28,.04)}input:hover,select:hover{border-color:#8f8794}input:focus,select:focus{border-color:#6941ff;outline:3px solid #eee9ff;outline-offset:1px}.connect-alert{margin-top:1.25rem;padding:.9rem 1rem;border:1px solid #fecaca;border-radius:.7rem;background:#fef2f2;color:#7f1d1d}.connect-alert strong{display:block;font-size:.9rem}.connect-alert p{margin:.25rem 0 0;color:#991b1b;font-size:.875rem;line-height:1.5}
  </style>
</head>
<body><main style="--connect-accent:{{.Branding.PrimaryColor}};--connect-accent-foreground:{{.Branding.AccentForeground}}">
  <header class="connect-brand">{{if .Branding.LogoURL}}<img class="connect-logo" src="{{.Branding.LogoURL}}" width="48" height="48" alt="" referrerpolicy="no-referrer">{{end}}<span>{{.Branding.DisplayName}}</span></header>
  {{if .Expired}}
    <p class="connect-eyebrow" data-tone="warning">Link unavailable</p>
    <h1>Connection link expired</h1><p class="connect-copy">Return to the application and start the connection again.</p>
  {{else}}
    <p class="connect-eyebrow">Secure connection</p>
    <h1>Connect your account</h1><p class="connect-copy">Enter the details needed to continue to your provider securely.</p>
    {{if .Error}}<div class="connect-alert" id="connect-form-error" role="alert"><strong>Check your connection details</strong><p>{{.Error}}</p></div>{{end}}
    <form class="connect-form" method="post" action="{{.Action}}" autocomplete="off" {{if .Error}}aria-describedby="connect-form-error connect-form-helper"{{else}}aria-describedby="connect-form-helper"{{end}}>
      <p class="connect-form-helper" id="connect-form-helper">These details select the correct provider account or site.</p>
      {{range .Fields}}
        <div class="connect-field">
          <label class="connect-label" for="field-{{.Name}}"><span>{{.Label}}</span><span class="connect-field-requirement">{{if .Required}}Required{{else}}Optional{{end}}</span></label>
          {{if .Select}}
            <select id="field-{{.Name}}" name="{{.Name}}" aria-describedby="{{if .Description}}{{.DescriptionID}}{{else}}connect-form-helper{{end}}" {{if .Required}}required{{end}}>
              <option value="" {{if not .Value}}selected{{end}}>{{if .Placeholder}}{{.Placeholder}}{{else}}Select an option{{end}}</option>
              {{range .Options}}<option value="{{.Value}}" {{if .Selected}}selected{{end}}>{{.Label}}</option>{{end}}
            </select>
          {{else}}
            <input type="text" id="field-{{.Name}}" name="{{.Name}}" value="{{.Value}}" {{if .Placeholder}}placeholder="{{.Placeholder}}"{{end}} autocapitalize="none" spellcheck="false" aria-describedby="{{if .Description}}{{.DescriptionID}}{{else}}connect-form-helper{{end}}" {{if .Required}}required{{end}}>
          {{end}}
          {{if .Description}}<p class="connect-field-description" id="{{.DescriptionID}}">{{.Description}}</p>{{end}}
        </div>
      {{end}}
      <button class="connect-action" type="submit">Continue</button>
    </form>
  {{end}}
  {{if or .Branding.SupportURL .Branding.PrivacyURL}}<footer class="connect-links">{{if .Branding.SupportURL}}<a href="{{.Branding.SupportURL}}" rel="noreferrer">Support</a>{{end}}{{if .Branding.PrivacyURL}}<a href="{{.Branding.PrivacyURL}}" rel="noreferrer">Privacy</a>{{end}}</footer>{{end}}
</main></body></html>`)

var connectInputProviderTemplate = parseHostedConnectTemplate("connect-input-provider", `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta http-equiv="refresh" content="0; url={{.AuthorizeURL}}">
  <title>Continue to provider · {{.Branding.DisplayName}}</title>
  {{template "hosted-connect-shell-style"}}
</head>
<body><main style="--connect-accent:{{.Branding.PrimaryColor}};--connect-accent-foreground:{{.Branding.AccentForeground}}">
  <div class="connect-brand">{{if .Branding.LogoURL}}<img class="connect-logo" src="{{.Branding.LogoURL}}" width="48" height="48" alt="" referrerpolicy="no-referrer">{{end}}<span>{{.Branding.DisplayName}}</span></div>
  <p class="connect-eyebrow">Authorization</p>
  <h1>Continue to your provider</h1>
  <p class="connect-copy">Authorization should open automatically. If it does not, continue using the button below.</p>
  <a class="connect-action" href="{{.AuthorizeURL}}" rel="noreferrer">Continue to provider</a>
  {{if or .Branding.SupportURL .Branding.PrivacyURL}}<div class="connect-links">{{if .Branding.SupportURL}}<a href="{{.Branding.SupportURL}}" rel="noreferrer">Support</a>{{end}}{{if .Branding.PrivacyURL}}<a href="{{.Branding.PrivacyURL}}" rel="noreferrer">Privacy</a>{{end}}</div>{{end}}
</main></body>
</html>`)

// ConnectInputPageHandler renders the short-lived Engine-owned collection
// page only for a valid pending form session. The raw token is a bearer secret,
// so the response is non-cacheable and telemetry records counts/outcomes only.
func ConnectInputPageHandler(s store.Store, verifier ServiceVerifier, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.connect.input.view")
		defer span.End()
		branding := loadHostedConnectBranding(ctx, s)
		loaded, err := loadConnectInputSession(ctx, s, verifier, masterKey, r.URL.Query().Get("token"))
		if err != nil {
			recordConnectInputOutcome(ctx, span, "view", connectInputOutcome(err), 0)
			writeConnectInputUnavailable(w, branding)
			return
		}
		page := newConnectInputPage(r.URL.Query().Get("token"), loaded.resolved.metadata.ConnectConfig.ResourceInput, loaded.resolved.credentials.RedirectURI, loaded.values, "", branding)
		recordConnectInputOutcome(ctx, span, "view", "rendered", len(page.Fields))
		writeConnectInputPage(w, http.StatusOK, page)
	}
}

// ConnectInputSubmitHandler validates the browser fields before creating any
// OAuth state. A successful submission atomically consumes the form session
// and inserts the provider callback session, then renders the provider handoff.
func ConnectInputSubmitHandler(s store.Store, verifier ServiceVerifier, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.connect.input.submit")
		defer span.End()
		branding := loadHostedConnectBranding(ctx, s)
		r.Body = http.MaxBytesReader(w, r.Body, maxConnectInputFormBytes)
		if err := r.ParseForm(); err != nil {
			recordConnectInputOutcome(ctx, span, "submit", "invalid", 0)
			writeConnectInputUnavailable(w, branding)
			return
		}
		// The capability remains in the action query rather than a form field,
		// preventing collisions with a provider profile that declares "token" as
		// legitimate customer routing input.
		token := strings.TrimSpace(r.URL.Query().Get("token"))
		loaded, err := loadConnectInputSession(ctx, s, verifier, masterKey, token)
		if err != nil {
			recordConnectInputOutcome(ctx, span, "submit", connectInputOutcome(err), 0)
			writeConnectInputUnavailable(w, branding)
			return
		}
		fields := loaded.resolved.metadata.ConnectConfig.ResourceInput.Fields
		values := connectInputFormValues(fields, r.PostForm)
		prepared, err := prepareConnectResourceInput(loaded.resolved.metadata.ConnectConfig, values)
		if err != nil || len(prepared.missing) != 0 {
			// Invalid supplied data is kept on the same form rather than creating
			// an OAuth session that would later route to an unusable tenant.
			page := newConnectInputPage(token, loaded.resolved.metadata.ConnectConfig.ResourceInput, loaded.resolved.credentials.RedirectURI, values, "Provide valid connection details to continue.", branding)
			recordConnectInputOutcome(ctx, span, "submit", "invalid", len(page.Fields))
			writeConnectInputPage(w, http.StatusBadRequest, page)
			return
		}
		response, err := completeConnectInputSession(ctx, s, loaded, prepared.canonical, masterKey)
		if err != nil {
			recordConnectInputOutcome(ctx, span, "submit", connectInputOutcome(err), len(fields))
			writeConnectInputUnavailable(w, branding)
			return
		}
		recordConnectInputOutcome(ctx, span, "submit", "provider_handoff", len(fields))
		writeConnectInputProviderPage(w, response.AuthorizeURL, branding)
	}
}

// loadConnectInputSession resolves one exact hashed token row and its pinned
// runtime profile. The lookup remains constant-count and rejects replay,
// expiry, or configuration drift before any customer values are displayed.
func loadConnectInputSession(ctx context.Context, s store.Store, verifier ServiceVerifier, masterKey []byte, rawToken string) (resolvedConnectInputSession, error) {
	tokenHash, err := connectInputTokenHash(rawToken)
	if err != nil {
		return resolvedConnectInputSession{}, err
	}
	session, err := s.GetActiveConnectInputSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		return resolvedConnectInputSession{}, err
	}
	if session == nil {
		return resolvedConnectInputSession{}, store.ErrConnectSessionUnavailable
	}
	call := connectAdminCall{bucketID: session.BucketID, serviceID: session.ServiceID}
	resolved, err := resolveConnectRuntimeConfig(ctx, s, verifier, call, masterKey)
	if err != nil {
		return resolvedConnectInputSession{}, err
	}
	if err := validateConnectInputSessionContract(ctx, s, session, resolved); err != nil {
		return resolvedConnectInputSession{}, err
	}
	values, err := decodeConnectInputValues(session.ResourceInputJSON)
	if err != nil {
		return resolvedConnectInputSession{}, store.ErrConnectSessionUnavailable
	}
	return resolvedConnectInputSession{session: session, resolved: resolved, values: values}, nil
}

// connectInputTokenHash validates the bounded browser capability before an
// indexed lookup and keeps the raw token out of every downstream structure.
func connectInputTokenHash(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if token == "" || len(token) > 256 {
		return "", store.ErrConnectSessionUnavailable
	}
	return connectHash(token), nil
}

// validateConnectInputSessionContract rejects same-name profile drift and
// repeats app/service scope admission at submit time. A pending form cannot
// silently inherit new auth URLs, fields, hosts, or permissions.
func validateConnectInputSessionContract(ctx context.Context, s store.Store, session *store.ConnectInputSession, resolved connectRuntimeConfig) error {
	if session.AuthType != resolved.config.AuthType || session.AuthName != resolved.config.AuthName || resolved.metadata.ConnectConfig == nil || resolved.metadata.ConnectConfig.ResourceInput == nil {
		return store.ErrConnectSessionUnavailable
	}
	contractHash, err := connectInputContractHash(resolved)
	if err != nil || contractHash != session.ContractHash {
		return store.ErrConnectSessionUnavailable
	}
	appScopes, err := applyAppConnectScopePolicy(ctx, s, session.BucketID, session.ServiceID, session.CreatedByAppID, session.RequestedScopes)
	if err != nil {
		return store.ErrConnectSessionUnavailable
	}
	effectiveScopes, err := resolveConnectScopes(resolved.auth, resolved.flow, appScopes)
	if err != nil || !slices.Equal(effectiveScopes, session.RequestedScopes) {
		return store.ErrConnectSessionUnavailable
	}
	return nil
}

// decodeConnectInputValues accepts only the canonical string map written by
// the start path, preventing malformed stored JSON from reaching form fields.
func decodeConnectInputValues(raw []byte) (map[string]string, error) {
	values := map[string]string{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

// completeConnectInputSession builds the provider request from already
// validated data, then delegates replay protection and callback-row insertion
// to one transaction owned by the store.
func completeConnectInputSession(ctx context.Context, s store.Store, loaded resolvedConnectInputSession, resourceInputJSON []byte, masterKey []byte) (connectSessionStartResponse, error) {
	session := loaded.session
	call := connectAdminCall{bucketID: session.BucketID, serviceID: session.ServiceID}
	providerSession, response, err := buildProviderConnectSession(call, session.EndUserRef, session.CreatedByAppID, session.ReturnURL, resourceInputJSON, session.RequestedScopes, loaded.resolved, masterKey)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	if _, err := s.CompleteConnectInputSession(ctx, session.TokenHash, session.ContractHash, time.Now().UTC(), providerSession); err != nil {
		return connectSessionStartResponse{}, err
	}
	return response, nil
}

// buildConnectInputURL uses the already-approved Engine callback origin so a
// provider redirect configuration cannot be repurposed into an arbitrary form
// host. Only the one-time raw token crosses the browser boundary.
func buildConnectInputURL(callbackURL, token string) (string, error) {
	parsed, err := url.Parse(callbackURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("connect callback URL is invalid")
	}
	parsed.Path = "/workspace/connect/input"
	parsed.RawPath = ""
	parsed.RawQuery = url.Values{"token": []string{token}}.Encode()
	parsed.Fragment = ""
	return parsed.String(), nil
}

// connectInputFormValues projects only declared fields from the submitted
// form. Browser control fields and attacker-added names never enter resource
// validation, routing metadata, storage, or telemetry.
func connectInputFormValues(fields []fusedobject.ResourceInputField, form url.Values) map[string]string {
	values := make(map[string]string, len(fields))
	for _, field := range fields {
		values[field.Name] = form.Get(field.Name)
	}
	return values
}

// newConnectInputPage maps versioned field declarations to presentation data
// without exposing bucket, app, end-user, provider URL, or OAuth identifiers.
func newConnectInputPage(token string, config *fusedobject.ResourceInputConfig, redirectURI string, values map[string]string, message string, branding hostedConnectBranding) connectInputPage {
	fields := make([]connectInputPageField, 0, len(config.Fields))
	for _, field := range config.Fields {
		fields = append(fields, newConnectInputPageField(field, values[field.Name]))
	}
	action := "/workspace/connect/input?" + url.Values{"token": []string{token}}.Encode()
	return connectInputPage{Action: action, FormOrigin: connectInputFormOrigin(redirectURI), Fields: fields, Error: message, Branding: branding}
}

// newConnectInputPageField maps the closed text/select contract to explicit
// native controls instead of passing profile-controlled HTML types through.
func newConnectInputPageField(field fusedobject.ResourceInputField, value string) connectInputPageField {
	label := strings.TrimSpace(field.Label)
	// The stable field name remains a usable fallback for older text profiles.
	if label == "" {
		label = field.Name
	}
	pageField := connectInputPageField{
		Name: field.Name, Label: label, Value: value, Placeholder: field.Placeholder,
		Description: field.Description, DescriptionID: "field-" + field.Name + "-description",
		Required: field.Required, Select: field.Type == "select",
	}
	// RE2 remains server-only; select values instead receive a closed option
	// projection whose membership is repeated by runtime validation.
	if pageField.Select {
		pageField.Options = connectInputPageOptions(field.Options, value)
	}
	return pageField
}

// connectInputPageOptions preserves declaration order and marks only the exact
// canonical value selected by the server-validated string contract.
func connectInputPageOptions(options []fusedobject.ResourceInputOption, selected string) []connectInputPageOption {
	pageOptions := make([]connectInputPageOption, 0, len(options))
	for _, option := range options {
		label := option.Label
		// An omitted presentation label displays the canonical submitted value.
		if label == "" {
			label = option.Value
		}
		pageOptions = append(pageOptions, connectInputPageOption{Value: option.Value, Label: label, Selected: option.Value == selected})
	}
	return pageOptions
}

// connectInputFormOrigin pins form submission to the owner-reviewed Engine
// callback origin without copying its path or query into CSP.
func connectInputFormOrigin(raw string) string {
	parsed, err := parseConnectAuthorizationURL(raw)
	// An invalid callback contract remains blocked by the self-only fallback
	// policy and cannot broaden where the browser may submit customer input.
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}

// writeConnectInputPage applies browser hardening uniformly to successful and
// validation-error forms because both responses contain a one-time bearer
// token and may contain customer-supplied non-secret routing values.
func writeConnectInputPage(w http.ResponseWriter, status int, page connectInputPage) {
	writeHostedConnectHTML(w, status, page.FormOrigin, page.Branding, connectInputTemplate, page)
}

// writeConnectInputProviderPage ends the form-submission navigation before
// starting provider navigation, so CSP cannot block a provider redirect chain.
func writeConnectInputProviderPage(w http.ResponseWriter, authorizeURL string, branding hostedConnectBranding) {
	page := connectInputProviderPage{AuthorizeURL: authorizeURL, Branding: branding}
	writeHostedConnectHTML(w, http.StatusOK, "", branding, connectInputProviderTemplate, page)
}

// writeHostedConnectHTML applies the shared hardened response envelope before
// executing one page-specific escaped template.
func writeHostedConnectHTML(w http.ResponseWriter, status int, formOrigin string, branding hostedConnectBranding, pageTemplate *template.Template, data any) {
	writeConnectInputSecurityHeaders(w, formOrigin, branding.LogoOrigin)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, data)
}

// writeConnectInputSecurityHeaders protects both rendered forms and provider
// redirects because each response follows a request carrying a bearer token.
func writeConnectInputSecurityHeaders(w http.ResponseWriter, formOrigin, logoOrigin string) {
	formAction := "form-action 'self'"
	// An explicit Engine origin covers browsers that do not treat localhost
	// consistently for the self source while retaining a closed form allowlist.
	if formOrigin != "" {
		formAction += " " + formOrigin
	}
	w.Header().Set("Cache-Control", "no-store")
	imgSource := "img-src 'self'"
	if logoOrigin != "" {
		// Only the validated exact logo origin is admitted; paths, queries, and
		// unrelated customer hosts never broaden the image policy.
		imgSource += " " + logoOrigin
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; "+imgSource+"; "+formAction+"; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// writeConnectInputUnavailable intentionally collapses missing, expired,
// replayed, and internally unavailable sessions to the same public response so
// the form endpoint cannot be used to enumerate Engine connection state.
func writeConnectInputUnavailable(w http.ResponseWriter, branding hostedConnectBranding) {
	writeConnectInputPage(w, http.StatusGone, connectInputPage{Expired: true, Branding: branding})
}

// loadHostedConnectBranding keeps connect pages available during a settings
// read failure and defensively rejects manually corrupted database values.
func loadHostedConnectBranding(ctx context.Context, s store.Store) hostedConnectBranding {
	fallback := store.DefaultConnectBranding()
	branding, err := s.GetConnectBranding(ctx)
	if err != nil {
		// Rendering remains available even if optional settings cannot be read.
		slog.WarnContext(ctx, "connect branding unavailable; using compiled fallback")
		return hostedConnectBranding{ConnectBranding: fallback}
	}
	validated, err := validateConnectBranding(branding)
	if err != nil {
		// Manual database corruption cannot broaden HTML or CSP browser authority.
		slog.WarnContext(ctx, "connect branding invalid; using compiled fallback")
		return hostedConnectBranding{ConnectBranding: fallback}
	}
	return hostedConnectBranding{ConnectBranding: validated, LogoOrigin: connectBrandingLogoOrigin(validated.LogoURL)}
}

// connectBrandingLogoOrigin reduces a validated logo URL to the exact CSP source.
func connectBrandingLogoOrigin(raw string) string {
	if raw == "" {
		// No configured logo needs no external CSP source.
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		// Defensive parsing failure preserves the self-only image policy.
		return ""
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}

// connectInputOutcome maps internal failures to a bounded telemetry enum;
// hashes, tokens, customer values, profile fields, URLs, and raw errors remain
// outside traces and audit dimensions.
func connectInputOutcome(err error) string {
	if errors.Is(err, store.ErrConnectSessionUnavailable) {
		return "unavailable"
	}
	return "failed"
}

// recordConnectInputOutcome annotates the one user-triggered view/submission
// span with only fixed action/outcome values and a bounded structural count.
func recordConnectInputOutcome(ctx context.Context, span trace.Span, action, outcome string, fieldCount int) {
	span.SetAttributes(
		attribute.String("connect.input.action", action),
		attribute.String("connect.input.outcome", outcome),
		attribute.Int("connect.input.field_count", fieldCount),
	)
	// The log mirrors OTEL's fixed allowlist so operators can diagnose browser
	// handoffs without recording customer values or bearer capabilities.
	slog.InfoContext(ctx, "connect input request completed",
		"action", action,
		"outcome", outcome,
		"field_count", fieldCount,
	)
}
