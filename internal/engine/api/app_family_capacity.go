package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// appFamilyCapacityPolicy keeps adapter-specific quota language beside the
// shared admission rule so SDK and MCP cannot drift into different semantics.
type appFamilyCapacityPolicy struct {
	kind        store.AppKind
	resource    string
	errorCode   string
	displayName string
	remediation string
	limit       *int
}

// enforceAppFamilyCapacity admits a target already occupying one quota unit
// and otherwise applies the adapter's entitlement before it becomes invokable.
func enforceAppFamilyCapacity(ctx context.Context, s store.Store, span trace.Span, accountID uuid.UUID, canonicalName string, policy appFamilyCapacityPolicy) error {
	usage, err := s.GetAppFamilyQuotaUsage(ctx, accountID, policy.kind.String(), canonicalName)
	// Quota admission fails closed when the current invokable usage is unavailable.
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"), attribute.String("error.code", "app_family_count_failed"))
		span.SetStatus(codes.Error, "app_family_count_failed")
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, code: "app_family_count_failed", message: "The Engine could not inspect current app-family usage.", category: "internal", retryable: true, remediation: "Retry and check Engine logs if the problem continues."}
	}
	// An upgrade inside a currently runnable family allocates no additional quota unit.
	if usage.TargetInvokable {
		return nil
	}
	// Entitlement telemetry records only bounded resource counts and never app identity.
	if limitErr := entitlement.CheckLimit(span, policy.resource, usage.CurrentInvokable, policy.limit); limitErr != nil {
		span.SetAttributes(attribute.String("outcome", policy.errorCode))
		return workspaceConfigHTTPError{
			status:      http.StatusForbidden,
			code:        policy.errorCode,
			message:     fmt.Sprintf("This workspace has reached its %s limit (%d of %d).", policy.displayName, limitErr.Current, limitErr.Limit),
			category:    "entitlement",
			details:     map[string]any{"current": limitErr.Current, "limit": limitErr.Limit},
			remediation: policy.remediation,
		}
	}
	return nil
}
