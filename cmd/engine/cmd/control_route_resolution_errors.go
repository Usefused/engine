package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// controlResolutionError is a reviewed response contract, not a wrapper around
// raw database errors or request bodies. Only its stable code enters auditing.
type controlResolutionError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Category    string `json:"category"`
	Retryable   bool   `json:"retryable"`
	Remediation string `json:"remediation"`
	Details     struct {
		ServerDetail string `json:"server_detail,omitempty"`
	} `json:"details"`
	status int
}

// Error keeps accidental logging bounded even when the HTTP detail names input keys.
func (e *controlResolutionError) Error() string { return e.Code }

// diagnosableControlResolution admits only explicitly reviewed failures to the
// diagnostic path; unknown errors remain ordinary fail-closed policy denials.
func diagnosableControlResolution(err error) bool {
	var diagnostic *controlResolutionError
	return err == nil || errors.As(err, &diagnostic)
}

// controlResolutionOutcome supplies only fixed outcome values to the existing span.
func controlResolutionOutcome(err error) string {
	var diagnostic *controlResolutionError
	// Successful resolution keeps the canonical authorization outcome unchanged.
	if !errors.As(err, &diagnostic) {
		return "allowed"
	}
	// Dependency outages must not be counted as caller validation errors.
	if diagnostic.Retryable {
		return "resolution_unavailable"
	}
	return "validation_failed"
}

// writeControlResolutionError records one failed control operation with no input
// names in audit or telemetry, then returns the bounded client diagnostic.
func writeControlResolutionError(w http.ResponseWriter, r *http.Request, recorder accesscontrol.AuditRecorder, actor accesscontrol.Actor, requirements []accesscontrol.Requirement, policy string, err error) {
	var diagnostic *controlResolutionError
	// Defensive admission ensures unexpected errors never gain a response body.
	if !errors.As(err, &diagnostic) {
		writeControlAuthorizationError(w, r, accesscontrol.ErrPolicyDenied)
		return
	}
	_ = recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, actor, controlAuditAction(r.Method), policy, requirements, accesscontrol.AuditFailed, diagnostic.status, diagnostic.Code))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(diagnostic.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": diagnostic})
}

// desiredServiceRequirements uses per-key resolution rather than row counts so
// aliases cannot hide a missing reference or authorize a different identity.
func desiredServiceRequirements(keys []string, resolved map[string]uuid.UUID, permission accesscontrol.Permission) ([]accesscontrol.Requirement, []string) {
	requirements := make([]accesscontrol.Requirement, 0, len(resolved))
	var unresolved []string
	seen := make(map[uuid.UUID]bool)
	// The sorted input preserves deterministic diagnostics and grant ordering.
	for _, key := range keys {
		id := resolved[key]
		// Missing and ambiguous references both lack an authoritative identity.
		if id == uuid.Nil {
			unresolved = append(unresolved, key)
			continue
		}
		// Multiple submitted aliases for one service require only one grant.
		if !seen[id] {
			requirements = append(requirements, accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: id}})
			seen[id] = true
		}
	}
	return requirements, unresolved
}

// appendDesiredServiceCandidateRequirements preserves the existing full grant
// boundary because plan handlers still support legacy display-name selection.
// Canonical slug priority must never authorize a different colliding identity.
func (r *storeBackedControlRequirementResolver) appendDesiredServiceCandidateRequirements(ctx context.Context, keys []string, requirements []accesscontrol.Requirement, permission accesscontrol.Permission) ([]accesscontrol.Requirement, error) {
	services, err := r.store.ListWorkspaceServices(ctx, keys)
	// A failed candidate lookup cannot establish the handler's complete authority.
	if err != nil {
		return nil, serviceResolutionUnavailable()
	}
	seen := make(map[accesscontrol.ResourceRef]bool)
	// Preserve previously resolved app/service/bucket grants without duplicates.
	for _, requirement := range requirements {
		seen[requirement.Resource] = true
	}
	// The existing SQL matcher returns the bounded candidate set in one batch.
	for _, service := range services {
		resource := accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service.ServiceID}
		// Colliding names require their own grant even if canonical lookup preferred a slug.
		if !seen[resource] {
			requirements = append(requirements, accesscontrol.Requirement{Permission: permission, Resource: resource})
			seen[resource] = true
		}
	}
	return requirements, nil
}

// serviceResolutionUnavailable distinguishes infrastructure failure without
// returning database text, connection details, or credentials.
func serviceResolutionUnavailable() *controlResolutionError {
	return &controlResolutionError{
		Code: "config_service_resolution_unavailable", Message: "Engine could not resolve the configured services.",
		Category: "dependency", Retryable: true, status: http.StatusServiceUnavailable,
		Remediation: "Retry the plan. If this persists, check Engine health.",
	}
}

// unresolvedConfigServices echoes only bounded caller-supplied references and
// directs discovery through the existing permission-filtered service list.
func unresolvedConfigServices(keys []string) *controlResolutionError {
	diagnostic := &controlResolutionError{
		Code: "config_service_unresolved", Message: "One or more configured service references could not be resolved unambiguously in this workspace.",
		Category: "validation", status: http.StatusBadRequest,
		Remediation: "Run `fused-cli workspace services list` to check available service slugs, update the config, and plan again.",
	}
	var refs []string
	// A small deterministic sample bounds response size independently of config size.
	for _, key := range keys {
		// Unsafe identifiers are omitted rather than echoed as terminal or credential material.
		if safeConfigServiceReference(key) {
			refs = append(refs, strconv.Quote(key))
		}
		// Five short references fit within the CLI's shared server-detail limit.
		if len(refs) == 5 {
			break
		}
	}
	// The generic message remains actionable when every supplied key is unsafe.
	if len(refs) > 0 {
		diagnostic.Details.ServerDetail = "Unresolved service references (up to 5 shown): " + strings.Join(refs, ", ") + "."
	}
	return diagnostic
}

var configServiceReferencePattern = regexp.MustCompile(`^@?[A-Za-z0-9][A-Za-z0-9._ /-]{0,127}$`)

// safeConfigServiceReference accepts ordinary slugs/display names but excludes
// URLs, terminal escapes, serialized credentials, and known credential prefixes.
func safeConfigServiceReference(value string) bool {
	lower := strings.ToLower(value)
	return configServiceReferencePattern.MatchString(value) && !strings.Contains(lower, "fsk_") && !strings.Contains(lower, "-----begin ")
}
