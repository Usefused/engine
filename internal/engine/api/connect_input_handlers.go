package api

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
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
	Name     string
	Label    string
	Value    string
	Required bool
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

type resolvedConnectInputSession struct {
	session  *store.ConnectInputSession
	resolved connectRuntimeConfig
	values   map[string]string
}

var connectInputTemplate = template.Must(template.New("connect-input").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Connect your account · {{.Branding.DisplayName}}</title>
  <style>
    :root{color-scheme:light dark;font-family:Inter,ui-sans-serif,system-ui,sans-serif}body{margin:0;background:#f7f7f8;color:#18181b}main{max-width:28rem;margin:8vh auto;padding:2rem;background:#fff;border:1px solid #e4e4e7;border-radius:1rem;box-shadow:0 12px 32px rgba(0,0,0,.08)}header{display:flex;align-items:center;gap:.75rem;margin-bottom:1.5rem}.brand-logo{width:48px;height:48px;object-fit:contain}.brand-name{font-weight:700;color:#18181b}h1{font-size:1.5rem;margin:0 0 .5rem}p{color:#52525b;line-height:1.5}label{display:block;font-weight:600;margin:1rem 0 .4rem}input{box-sizing:border-box;width:100%;padding:.75rem;border:1px solid #a1a1aa;border-radius:.5rem;font:inherit}button{width:100%;margin-top:1.5rem;padding:.8rem;border:0;border-radius:.5rem;color:#fff;font:inherit;font-weight:700;cursor:pointer}footer{display:flex;gap:1rem;margin-top:1.5rem;font-size:.875rem}footer a{color:inherit}@media(prefers-color-scheme:dark){body{background:#09090b;color:#fafafa}main{background:#18181b;border-color:#3f3f46}p{color:#d4d4d8}.brand-name{color:#fafafa}}
  </style>
</head>
<body><main>
  <header>{{if .Branding.LogoURL}}<img class="brand-logo" src="{{.Branding.LogoURL}}" width="48" height="48" alt="{{.Branding.DisplayName}} logo" referrerpolicy="no-referrer">{{end}}<span class="brand-name">{{.Branding.DisplayName}}</span></header>
  {{if .Expired}}
    <h1>Connection link expired</h1><p>Return to the application and start the connection again.</p>
  {{else}}
    <h1>Connection details</h1><p>Provide the details needed to send you to the provider's authorization page.</p>
    {{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
    <form method="post" action="{{.Action}}" autocomplete="off">
      {{range .Fields}}
        <label for="field-{{.Name}}">{{.Label}}</label>
        <input id="field-{{.Name}}" name="{{.Name}}" value="{{.Value}}" {{if .Required}}required{{end}}>
      {{end}}
      <button type="submit" style="background-color:{{.Branding.PrimaryColor}}">Continue</button>
    </form>
  {{end}}
  {{if or .Branding.SupportURL .Branding.PrivacyURL}}<footer>{{if .Branding.SupportURL}}<a href="{{.Branding.SupportURL}}" rel="noreferrer">Support</a>{{end}}{{if .Branding.PrivacyURL}}<a href="{{.Branding.PrivacyURL}}" rel="noreferrer">Privacy</a>{{end}}</footer>{{end}}
</main></body></html>`))

var connectInputProviderTemplate = template.Must(template.New("connect-input-provider").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta http-equiv="refresh" content="0; url={{.AuthorizeURL}}">
  <title>Continue to provider · {{.Branding.DisplayName}}</title>
  <style>:root{color-scheme:light dark;font-family:Inter,ui-sans-serif,system-ui,sans-serif}body{margin:0;background:#f7f7f8;color:#18181b}main{max-width:28rem;margin:8vh auto;padding:2rem;background:#fff;border:1px solid #e4e4e7;border-radius:1rem}.brand{display:flex;align-items:center;gap:.75rem;font-weight:700;margin-bottom:1.5rem}.brand img{width:48px;height:48px;object-fit:contain}.continue{display:block;margin-top:1.5rem;padding:.8rem;border-radius:.5rem;color:#fff;text-align:center;text-decoration:none;font-weight:700}.links{display:flex;gap:1rem;margin-top:1.5rem;font-size:.875rem}.links a{color:inherit}@media(prefers-color-scheme:dark){body{background:#09090b;color:#fafafa}main{background:#18181b;border-color:#3f3f46}}</style>
</head>
<body><main>
  <div class="brand">{{if .Branding.LogoURL}}<img src="{{.Branding.LogoURL}}" width="48" height="48" alt="{{.Branding.DisplayName}} logo" referrerpolicy="no-referrer">{{end}}<span>{{.Branding.DisplayName}}</span></div>
  <h1>Continue to provider</h1>
  <p>If the provider does not open automatically, use the button below.</p>
  <a class="continue" href="{{.AuthorizeURL}}" rel="noreferrer" style="background-color:{{.Branding.PrimaryColor}}">Continue to provider</a>
  {{if or .Branding.SupportURL .Branding.PrivacyURL}}<div class="links">{{if .Branding.SupportURL}}<a href="{{.Branding.SupportURL}}" rel="noreferrer">Support</a>{{end}}{{if .Branding.PrivacyURL}}<a href="{{.Branding.PrivacyURL}}" rel="noreferrer">Privacy</a>{{end}}</div>{{end}}
</main></body>
</html>`))

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
		label := strings.TrimSpace(field.Label)
		if label == "" {
			label = field.Name
		}
		// RE2 is the server-side validation contract; HTML pattern uses a
		// different JavaScript grammar and could reject otherwise valid values.
		fields = append(fields, connectInputPageField{Name: field.Name, Label: label, Value: values[field.Name], Required: field.Required})
	}
	action := "/workspace/connect/input?" + url.Values{"token": []string{token}}.Encode()
	return connectInputPage{Action: action, FormOrigin: connectInputFormOrigin(redirectURI), Fields: fields, Error: message, Branding: branding}
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
	writeConnectInputSecurityHeaders(w, page.FormOrigin, page.Branding.LogoOrigin)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = connectInputTemplate.Execute(w, page)
}

// writeConnectInputProviderPage ends the form-submission navigation before
// starting provider navigation, so CSP cannot block a provider redirect chain.
func writeConnectInputProviderPage(w http.ResponseWriter, authorizeURL string, branding hostedConnectBranding) {
	writeConnectInputSecurityHeaders(w, "", branding.LogoOrigin)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = connectInputProviderTemplate.Execute(w, connectInputProviderPage{AuthorizeURL: authorizeURL, Branding: branding})
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
