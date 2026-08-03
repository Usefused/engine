package accesscontrol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

var ErrUnsafeAuditMetadata = errors.New("unsafe audit metadata")

const MaxAuditMissingRequirements = 64

func ValidateAuditableRequirementCount(requirements []Requirement) error {
	if len(requirements) > MaxAuditMissingRequirements {
		return fmt.Errorf("auditable authorization requirements exceed %d", MaxAuditMissingRequirements)
	}
	return nil
}

type AuditOutcome string

const (
	AuditAttempted AuditOutcome = "attempted"
	AuditAllowed   AuditOutcome = "allowed"
	AuditDenied    AuditOutcome = "denied"
	AuditSucceeded AuditOutcome = "succeeded"
	AuditFailed    AuditOutcome = "failed"
)

type AuditEvent struct {
	ID                  uuid.UUID
	OccurredAt          time.Time
	ActorSubjectID      uuid.UUID
	ActorCredentialID   uuid.UUID
	Action              string
	Permission          Permission
	Resource            ResourceRef
	MissingRequirements []Requirement
	RequestID           string
	TraceID             string
	Method              string
	Path                string
	Outcome             AuditOutcome
	StatusCode          int
	ReasonCode          string
	SourceIP            string
	UserAgent           string
	Metadata            map[string]any
}

type AuditRecorder interface {
	RecordAuthorizationAudit(ctx context.Context, event AuditEvent) error
}

func (event AuditEvent) Validate() error {
	if err := validateAuditText("action", event.Action, 128, true); err != nil {
		return err
	}
	if !validAuditOutcome(event.Outcome) {
		return fmt.Errorf("invalid audit outcome %q", event.Outcome)
	}
	if err := validateAuditPermissionResource(event); err != nil {
		return err
	}
	if err := validateAuditMissingRequirements(event); err != nil {
		return err
	}
	if err := validateAuditRequestContext(event); err != nil {
		return err
	}
	_, err := SanitizeAuditMetadata(event.Metadata)
	return err
}

func validateAuditMissingRequirements(event AuditEvent) error {
	if err := ValidateAuditableRequirementCount(event.MissingRequirements); err != nil {
		return err
	}
	if len(event.MissingRequirements) > 0 && event.Outcome != AuditDenied {
		return fmt.Errorf("missing audit requirements require a denied outcome")
	}
	for _, requirement := range event.MissingRequirements {
		if err := validateRequirement(requirement); err != nil {
			return err
		}
	}
	return nil
}

func validateAuditPermissionResource(event AuditEvent) error {
	if event.Permission != "" {
		if err := ValidatePermission(event.Permission); err != nil {
			return err
		}
	}
	if event.Resource.Type != "" {
		if err := ValidateResourceType(event.Resource.Type); err != nil {
			return err
		}
		if event.Resource.ID == uuid.Nil {
			return fmt.Errorf("audit resource ID is required")
		}
	}
	return nil
}

func validateAuditRequestContext(event AuditEvent) error {
	for _, field := range []struct {
		name     string
		value    string
		max      int
		required bool
	}{
		{"request_id", event.RequestID, 128, false},
		{"trace_id", event.TraceID, 64, false},
		{"method", event.Method, 16, false},
		{"path", event.Path, 256, false},
		{"reason_code", event.ReasonCode, 64, false},
		{"source_ip", event.SourceIP, 64, false},
	} {
		if err := validateAuditText(field.name, field.value, field.max, field.required); err != nil {
			return err
		}
	}
	// Raw user agents are high-cardinality, user-controlled data and can carry
	// arbitrary secrets. Device analytics can be added later as bounded enums.
	if event.UserAgent != "" {
		return fmt.Errorf("%w: raw user_agent is not allowed", ErrUnsafeAuditMetadata)
	}
	if strings.Contains(event.Path, "?") {
		return fmt.Errorf("%w: audit path must not include a query string", ErrUnsafeAuditMetadata)
	}
	if event.StatusCode < 0 || event.StatusCode > 599 {
		return fmt.Errorf("invalid audit status code %d", event.StatusCode)
	}
	return nil
}

func validateAuditText(name, value string, max int, required bool) error {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return fmt.Errorf("audit %s is required", name)
	}
	if value == "" {
		return nil
	}
	if trimmed != value || len(value) > max || credentialShapedString(value) {
		return fmt.Errorf("%w: invalid %s", ErrUnsafeAuditMetadata, name)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: invalid %s", ErrUnsafeAuditMetadata, name)
		}
	}
	return nil
}

func validAuditOutcome(outcome AuditOutcome) bool {
	switch outcome {
	case AuditAttempted, AuditAllowed, AuditDenied, AuditSucceeded, AuditFailed:
		return true
	default:
		return false
	}
}

var allowedAuditMetadata = map[string]struct{}{
	"authorization_revision": {},
	"requirements":           {},
	"root_fields":            {},
	"cache_hit":              {},
	"role_count":             {},
	"team_count":             {},
	"binding_count":          {},
	"member_count":           {},
	"permission_count":       {},
	"service_count":          {},
	"bucket_count":           {},
	"artifact_count":         {},
	"selection_count":        {},
	"team_id":                {},
	"role_slug":              {},
	"changed":                {},
	"user_id":                {},
	"credential_id":          {},
	"membership_role":        {},
	"created_user":           {},
	// Artifact ownership is identity metadata, not credential material. These
	// fields are emitted by the atomic scope transaction and must remain in the
	// same allowlist used when records are read back through GraphQL.
	"owner_type": {},
	"owner_id":   {},
}

// SanitizeAuditMetadata returns a shallow copy after rejecting fields that
// could persist credentials, secret material, or request/response payloads.
// Callers must pass display-safe scalar metadata, not arbitrary bodies.
func SanitizeAuditMetadata(metadata map[string]any) (map[string]any, error) {
	if len(metadata) > len(allowedAuditMetadata) {
		return nil, fmt.Errorf("%w: too many fields", ErrUnsafeAuditMetadata)
	}
	sanitized := make(map[string]any, len(metadata))
	for key, value := range metadata {
		if _, allowed := allowedAuditMetadata[key]; !allowed || unsafeAuditValue(value) {
			return nil, fmt.Errorf("%w: field %q", ErrUnsafeAuditMetadata, key)
		}
		sanitized[key] = value
	}
	return sanitized, nil
}

func unsafeAuditValue(value any) bool {
	if value == nil {
		return false
	}
	text, ok := value.(string)
	if !ok {
		return !safeAuditScalar(value)
	}
	return validateAuditText("metadata value", text, 128, false) != nil
}

func safeAuditScalar(value any) bool {
	switch value.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return true
	default:
		// Composite values are rejected so an apparently safe top-level key
		// cannot hide request bodies or nested credential fields.
		return false
	}
}

func credentialShapedString(text string) bool {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "bearer ") || strings.HasPrefix(lower, "basic ") {
		return true
	}
	if strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "fsk_") || strings.HasPrefix(lower, "fused_") {
		return true
	}
	// Three non-empty dot-separated segments are the stable shape of a JWT.
	parts := strings.Split(trimmed, ".")
	return len(parts) == 3 && len(trimmed) > 30 && strings.HasPrefix(parts[0], "eyJ") && strings.HasPrefix(parts[1], "eyJ")
}
