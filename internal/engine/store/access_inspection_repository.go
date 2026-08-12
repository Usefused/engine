package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var (
	ErrAccessExplanationHidden = errors.New("access explanation is unavailable")
	ErrInvalidAuditQuery       = errors.New("invalid audit query")
)

type AccessExplanationQuery struct {
	RequesterSubjectID uuid.UUID
	TargetSubjectID    uuid.UUID
	Requirement        accesscontrol.Requirement
}

type AccessGrantSource struct {
	PrincipalType string
	PrincipalID   uuid.UUID
	TeamName      string
	RoleSlug      string
	Resource      accesscontrol.ResourceRef
}

type AccessExplanation struct {
	Requirement accesscontrol.Requirement
	Allowed     bool
	Sources     []AccessGrantSource
	Missing     []accesscontrol.Requirement
}

type AuditCursor struct {
	OccurredAt time.Time
	ID         uuid.UUID
}

type AuditQuery struct {
	RequesterSubjectID uuid.UUID
	ActorSubjectID     *uuid.UUID
	Actions            []string
	Outcomes           []accesscontrol.AuditOutcome
	From               *time.Time
	To                 *time.Time
	After              *AuditCursor
	Limit              int
}

type AuditRecord struct {
	ID                  uuid.UUID
	OccurredAt          time.Time
	ActorSubjectID      *uuid.UUID
	ActorCredentialID   *uuid.UUID
	Action              string
	Permission          accesscontrol.Permission
	Resource            accesscontrol.ResourceRef
	MissingRequirements []accesscontrol.Requirement
	RequestID           string
	TraceID             string
	Method              string
	Path                string
	Outcome             accesscontrol.AuditOutcome
	StatusCode          int
	ReasonCode          string
	Metadata            map[string]any
}

type AuditPage struct {
	Items      []AuditRecord
	Total      int
	NextCursor *AuditCursor
}

type AuditExportQuery struct {
	RequesterSubjectID uuid.UUID
	ActorSubjectID     *uuid.UUID
	Actions            []string
	Outcomes           []accesscontrol.AuditOutcome
	From               *time.Time
	To                 *time.Time
	Limit              int
}

// AuditExportRow deliberately omits metadata, source IP, user-agent, and all
// request/response bodies so CSV/JSONL exports are safe by construction.
type AuditExportRow struct {
	ID                  uuid.UUID
	OccurredAt          time.Time
	ActorSubjectID      *uuid.UUID
	ActorCredentialID   *uuid.UUID
	Action              string
	Permission          accesscontrol.Permission
	Resource            accesscontrol.ResourceRef
	MissingRequirements []accesscontrol.Requirement
	RequestID           string
	TraceID             string
	Method              string
	Path                string
	Outcome             accesscontrol.AuditOutcome
	StatusCode          int
	ReasonCode          string
}

type AccessInspectionRepository interface {
	ExplainAccess(context.Context, AccessExplanationQuery) (AccessExplanation, error)
}

type AuditRepository interface {
	QueryAuditEvents(context.Context, AuditQuery) (AuditPage, error)
	ExportAuditEvents(context.Context, AuditExportQuery) ([]AuditExportRow, error)
}

func validateExplanationQuery(input AccessExplanationQuery) error {
	if input.RequesterSubjectID == uuid.Nil || input.TargetSubjectID == uuid.Nil || input.Requirement.Resource.ID == uuid.Nil {
		return ErrAccessExplanationHidden
	}
	if err := accesscontrol.ValidatePermission(input.Requirement.Permission); err != nil {
		return ErrAccessExplanationHidden
	}
	if err := accesscontrol.ValidateResourceType(input.Requirement.Resource.Type); err != nil {
		return ErrAccessExplanationHidden
	}
	return nil
}

func validateAuditQuery(requester uuid.UUID, actions []string, outcomes []accesscontrol.AuditOutcome, from, to *time.Time, limit, maxLimit int) error {
	if requester == uuid.Nil || limit < 1 || limit > maxLimit || (from != nil && to != nil && from.After(*to)) {
		return ErrInvalidAuditQuery
	}
	if err := validateAuditActions(actions); err != nil {
		return err
	}
	return validateAuditOutcomes(outcomes)
}

func validateAuditActions(actions []string) error {
	for _, action := range actions {
		if strings.TrimSpace(action) != action || action == "" || len(action) > 128 {
			return ErrInvalidAuditQuery
		}
	}
	return nil
}

func validateAuditOutcomes(outcomes []accesscontrol.AuditOutcome) error {
	for _, outcome := range outcomes {
		switch outcome {
		case accesscontrol.AuditAttempted, accesscontrol.AuditAllowed, accesscontrol.AuditDenied, accesscontrol.AuditSucceeded, accesscontrol.AuditFailed, accesscontrol.AuditRolledBack, accesscontrol.AuditCancelled:
		default:
			return ErrInvalidAuditQuery
		}
	}
	return nil
}

func sanitizeStoredAuditMetadata(raw json.RawMessage) (map[string]any, error) {
	metadata := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, fmt.Errorf("decode stored audit metadata: %w", err)
		}
	}
	return accesscontrol.SanitizeAuditMetadata(metadata)
}
