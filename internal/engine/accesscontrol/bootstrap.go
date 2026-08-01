package accesscontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var ErrInvalidBootstrap = errors.New("invalid access-control bootstrap")

type BootstrapInput struct {
	AccountID        uuid.UUID
	CredentialHash   string
	CredentialPrefix string
	Roles            []RoleDefinition
	TraceID          string
}

type BootstrapResult struct {
	WorkspaceID  uuid.UUID
	SubjectID    uuid.UUID
	CredentialID uuid.UUID
	Revision     int64
	Changed      bool
}

type BootstrapRepository interface {
	ReconcileBootstrapOwner(ctx context.Context, input BootstrapInput) (BootstrapResult, error)
}

func BootstrapOwner(ctx context.Context, repository BootstrapRepository, accountID uuid.UUID, licenseKey string) (BootstrapResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.bootstrap_owner")
	defer span.End()

	if repository == nil || accountID == uuid.Nil || licenseKey == "" {
		err := fmt.Errorf("%w: repository, account ID, and FUSED_LICENSE_KEY are required", ErrInvalidBootstrap)
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid bootstrap input")
		return BootstrapResult{}, err
	}
	roles := BuiltInRoles()
	for _, role := range roles {
		if err := ValidateRoleDefinition(role); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "invalid built-in role")
			return BootstrapResult{}, err
		}
	}

	result, err := repository.ReconcileBootstrapOwner(ctx, BootstrapInput{
		AccountID:        accountID,
		CredentialHash:   HashControlCredential(licenseKey),
		CredentialPrefix: CredentialPrefix(licenseKey),
		Roles:            roles,
		TraceID:          traceIDFromContext(ctx),
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "bootstrap failed")
		return BootstrapResult{}, err
	}
	span.SetAttributes(
		attribute.Bool("engine.access.changed", result.Changed),
		attribute.Int64("engine.authorization.revision", result.Revision),
	)
	return result, nil
}

func HashControlCredential(credential string) string {
	hash := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(hash[:])
}

func CredentialPrefix(credential string) string {
	const prefixLength = 8
	if len(credential) <= prefixLength {
		// A short credential has no safe display prefix because that would store
		// the complete secret. Use a non-reversible fingerprint instead.
		return HashControlCredential(credential)[:prefixLength]
	}
	return credential[:prefixLength]
}

func traceIDFromContext(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}
