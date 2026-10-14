package cmd

import (
	"errors"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"net/http"
	"strings"
)

// hasCredentialRevealRequirement gives POST disclosures the final-audit gate normally used for sensitive GETs.
func hasCredentialRevealRequirement(requirements []accesscontrol.Requirement) bool {
	for _, requirement := range requirements {
		// This dedicated permission is the only route capability that returns plaintext bucket credentials.
		if requirement.Permission == accesscontrol.PermissionCredentialsReveal {
			return true
		}
	}
	return false
}

// auditSecretID admits only the UUID segment of the two reviewed secret routes, including authorization denials.
func auditSecretID(rawPath, policy string) (uuid.UUID, error) {
	// Audit policy templates are trusted; arbitrary URLs cannot add secret identity metadata.
	if !strings.Contains(policy, "/buckets/{bucket_id}/secrets/{secret_id}/") {
		return uuid.Nil, errors.New("not a secret route")
	}
	parts := strings.Split(strings.Trim(rawPath, "/"), "/")
	// Exact route length avoids guessing an identifier from an unrelated request.
	if len(parts) != 6 {
		return uuid.Nil, errors.New("invalid secret route")
	}
	return uuid.Parse(parts[4])
}

// applySecretExportAuditEvidence records the resulting policy in the existing mutation audit without retaining any material.
func applySecretExportAuditEvidence(event *accesscontrol.AuditEvent, evidence accesscontrol.MutationAuditEvidence) {
	// Unrelated mutations retain their existing metadata shape.
	if evidence.SecretExportRecorded {
		event.Metadata["exportable"] = evidence.SecretExportable
	}
}

// controlRequestAuditMode treats CSRF-protected POST reveals as sensitive reads, preserving fail-closed finalization.
func controlRequestAuditMode(r *http.Request, requirements []accesscontrol.Requirement) (bool, bool) {
	// Plaintext disclosure must never inherit best-effort mutation response delivery.
	if hasCredentialRevealRequirement(requirements) {
		return false, true
	}
	mutation := isControlMutation(r.Method)
	return mutation, !mutation && requiresSensitiveReadAudit(requirements)
}
