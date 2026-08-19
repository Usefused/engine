package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxConnectBrandingBodyBytes   = 32 << 10
	maxConnectDisplayNameRunes    = 100
	maxConnectBrandingURLBytes    = 2048
	connectBrandingUpdateAction   = "connect_branding.update"
	connectBrandingSuccessOutcome = "succeeded"
)

var connectBrandingColorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
var connectBrandingDNSPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
var connectBrandingPortPattern = regexp.MustCompile(`^[0-9]{1,5}$`)

// connectBrandingChanges is the bounded mutation result shared by OTEL and
// durable control audit without retaining any submitted values.
type connectBrandingChanges struct {
	DisplayName  bool
	LogoURL      bool
	PrimaryColor bool
	SupportURL   bool
	PrivacyURL   bool
}

// Count returns the fixed-width number of fields changed by one replacement.
func (changes connectBrandingChanges) Count() int {
	count := 0
	for _, changed := range []bool{changes.DisplayName, changes.LogoURL, changes.PrimaryColor, changes.SupportURL, changes.PrivacyURL} {
		if changed {
			// Each fixed field contributes at most one to the bounded aggregate.
			count++
		}
	}
	return count
}

// GetConnectBrandingHandler returns the singleton Engine branding projection.
func GetConnectBrandingHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		branding, err := s.GetConnectBranding(r.Context())
		if err != nil {
			// Database details remain in the storage boundary rather than logs that
			// may be exported alongside customer-controlled presentation data.
			slog.ErrorContext(r.Context(), "failed to load connect branding")
			http.Error(w, "failed to load connect branding", http.StatusInternalServerError)
			return
		}
		writeConnectJSON(w, http.StatusOK, branding)
	}
}

// UpsertConnectBrandingHandler validates and atomically replaces the Engine's
// hosted-connect identity while recording only boolean change facts.
func UpsertConnectBrandingHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.connect_branding.update")
		defer span.End()
		recordConnectBrandingChanges(span, connectBrandingChanges{}, "failed", "internal_error")
		span.SetAttributes(attribute.String("actor.type", "unknown"))
		actor, ok := accesscontrol.ActorFromContext(ctx)
		if !ok {
			// Direct invocation without the control middleware fails closed and
			// remains distinguishable from validation or persistence failures.
			recordConnectBrandingChanges(span, connectBrandingChanges{}, "denied", "unauthorized")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		span.SetAttributes(attribute.String("actor.type", string(actor.Kind)))

		branding, err := decodeConnectBranding(w, r)
		if err != nil {
			// Validation errors expose only the fixed request class in telemetry.
			recordConnectBrandingChanges(span, connectBrandingChanges{}, "invalid", "invalid_request")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		existing, err := s.GetConnectBranding(ctx)
		if err != nil {
			// Raw database errors can contain row or connection details and stay out
			// of logs and exported telemetry.
			recordConnectBrandingChanges(span, connectBrandingChanges{}, "failed", "branding_load_failed")
			slog.ErrorContext(ctx, "failed to load connect branding before update")
			http.Error(w, "failed to update connect branding", http.StatusInternalServerError)
			return
		}
		changes := compareConnectBranding(existing, branding)
		recordConnectBrandingChanges(span, changes, connectBrandingSuccessOutcome, "none")
		accesscontrol.MarkConnectBrandingAuditChanges(ctx, accesscontrol.ConnectBrandingAuditChanges{
			DisplayName: changes.DisplayName, LogoURL: changes.LogoURL, PrimaryColor: changes.PrimaryColor,
			SupportURL: changes.SupportURL, PrivacyURL: changes.PrivacyURL, Count: changes.Count(),
		})
		if changes.Count() == 0 {
			// A converged PUT remains successful and avoids an unnecessary database write.
			accesscontrol.MarkMutationAuditUnchanged(ctx)
			writeConnectJSON(w, http.StatusOK, existing)
			return
		}
		saved, err := s.UpsertConnectBranding(ctx, branding)
		if err != nil {
			// The attempted change shape remains visible while the raw store error
			// and submitted values remain absent.
			recordConnectBrandingChanges(span, changes, "failed", "branding_write_failed")
			slog.ErrorContext(ctx, "failed to update connect branding")
			http.Error(w, "failed to update connect branding", http.StatusInternalServerError)
			return
		}
		writeConnectJSON(w, http.StatusOK, saved)
	}
}

// decodeConnectBranding accepts one bounded strict JSON document so unknown
// settings cannot be silently ignored by older Engines.
func decodeConnectBranding(w http.ResponseWriter, r *http.Request) (store.ConnectBranding, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConnectBrandingBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var branding store.ConnectBranding
	if err := decoder.Decode(&branding); err != nil {
		// Malformed, oversized, or type-invalid JSON shares one stable response.
		return store.ConnectBranding{}, errors.New("invalid request body")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		// Rejecting trailing documents prevents ambiguous partial application.
		return store.ConnectBranding{}, errors.New("request body must contain one JSON object")
	}
	return validateConnectBranding(branding)
}

// validateConnectBranding normalizes harmless surrounding whitespace and
// rejects values that cannot be placed safely in HTML, CSS, or CSP.
func validateConnectBranding(branding store.ConnectBranding) (store.ConnectBranding, error) {
	branding.DisplayName = strings.TrimSpace(branding.DisplayName)
	branding.LogoURL = strings.TrimSpace(branding.LogoURL)
	branding.PrimaryColor = strings.TrimSpace(branding.PrimaryColor)
	branding.SupportURL = strings.TrimSpace(branding.SupportURL)
	branding.PrivacyURL = strings.TrimSpace(branding.PrivacyURL)
	if branding.DisplayName == "" || utf8.RuneCountInString(branding.DisplayName) > maxConnectDisplayNameRunes || containsControl(branding.DisplayName) {
		// A visible bounded name remains useful in headings, titles, and alt text.
		return store.ConnectBranding{}, errors.New("display_name must contain 1 to 100 visible characters")
	}
	if !connectBrandingColorPattern.MatchString(branding.PrimaryColor) {
		// The closed color grammar makes inline CSS rendering deterministic.
		return store.ConnectBranding{}, errors.New("primary_color must use #RRGGBB format")
	}
	for name, raw := range map[string]string{
		"logo_url": branding.LogoURL, "support_url": branding.SupportURL, "privacy_url": branding.PrivacyURL,
	} {
		if err := validateOptionalHTTPSURL(raw); err != nil {
			// The field name is safe server-owned context; the rejected value stays out.
			return store.ConnectBranding{}, errors.New(name + " must be an absolute HTTPS URL")
		}
	}
	return branding, nil
}

// validateOptionalHTTPSURL limits externally hosted assets and links to an
// absolute HTTPS authority whose host is safe to copy into a CSP source.
func validateOptionalHTTPSURL(raw string) error {
	if raw == "" {
		// Empty optional links preserve the compiled no-external-resource default.
		return nil
	}
	if len(raw) > maxConnectBrandingURLBytes || containsControl(raw) {
		// Bounded visible input cannot smuggle a second browser-header directive.
		return errors.New("invalid URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		// Parse failures never reach rendering or CSP derivation.
		return errors.New("invalid URL")
	}
	for _, invalid := range []bool{
		parsed.Scheme != "https", parsed.Host == "", parsed.User != nil,
		parsed.Opaque != "", parsed.Hostname() == "",
	} {
		if invalid {
			// Every URL must have one ordinary HTTPS authority without userinfo.
			return errors.New("invalid URL")
		}
	}
	if !validConnectBrandingHost(parsed.Hostname()) || !validConnectBrandingPort(parsed.Port()) {
		// CSP admits only a syntactically closed host and optional numeric port.
		return errors.New("invalid URL")
	}
	return nil
}

// validConnectBrandingHost accepts IP literals and DNS labels while excluding
// punctuation that could escape the single CSP source expression.
func validConnectBrandingHost(host string) bool {
	if net.ParseIP(host) != nil {
		// Standard IP literals are already constrained by net.ParseIP's grammar.
		return true
	}
	if !connectBrandingDNSPattern.MatchString(host) {
		// The coarse pattern removes all CSP punctuation before label inspection.
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if !validConnectBrandingDNSLabel(label) {
			// Per-label bounds retain ordinary DNS semantics and reject empty labels.
			return false
		}
	}
	return true
}

// validConnectBrandingDNSLabel enforces DNS length and edge rules after the
// whole-host character grammar has already been checked.
func validConnectBrandingDNSLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 {
		// Empty and oversized labels are not valid DNS authorities for CSP.
		return false
	}
	return label[0] != '-' && label[len(label)-1] != '-'
}

// validConnectBrandingPort rejects malformed or out-of-range explicit ports.
func validConnectBrandingPort(port string) bool {
	if port == "" {
		// Omitting a port uses the standard HTTPS port and needs no CSP suffix.
		return true
	}
	if !connectBrandingPortPattern.MatchString(port) {
		// A numeric bounded port cannot inject path or directive punctuation.
		return false
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		// Conversion failures stay invalid even after the numeric grammar check.
		return false
	}
	return value > 0 && value <= 65535
}

// containsControl rejects invisible delimiters before a value reaches a
// browser header, document, or durable setting.
func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

// compareConnectBranding produces a fixed boolean diff without retaining URLs
// or customer-visible text in telemetry or audit records.
func compareConnectBranding(current, next store.ConnectBranding) connectBrandingChanges {
	return connectBrandingChanges{
		DisplayName: current.DisplayName != next.DisplayName, LogoURL: current.LogoURL != next.LogoURL,
		PrimaryColor: current.PrimaryColor != next.PrimaryColor, SupportURL: current.SupportURL != next.SupportURL,
		PrivacyURL: current.PrivacyURL != next.PrivacyURL,
	}
}

// recordConnectBrandingChanges applies the exact low-cardinality mutation
// allowlist to the current span.
func recordConnectBrandingChanges(span trace.Span, changes connectBrandingChanges, outcome, errorCode string) {
	span.SetAttributes(
		attribute.String("connect_branding.action", connectBrandingUpdateAction),
		attribute.String("connect_branding.outcome", outcome),
		attribute.String("connect_branding.error_code", errorCode),
		attribute.Int("connect_branding.changed_field_count", changes.Count()),
		attribute.Bool("connect_branding.display_name_changed", changes.DisplayName),
		attribute.Bool("connect_branding.logo_url_changed", changes.LogoURL),
		attribute.Bool("connect_branding.primary_color_changed", changes.PrimaryColor),
		attribute.Bool("connect_branding.support_url_changed", changes.SupportURL),
		attribute.Bool("connect_branding.privacy_url_changed", changes.PrivacyURL),
	)
}
