package sandbox

import (
	"context"
	"errors"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

const (
	mcpEndUserRefRequiredCode    = "MCP_END_USER_REF_REQUIRED"
	mcpEndUserRefRequiredMessage = mcpEndUserRefRequiredCode + ": This operation requires connected OAuth/OIDC credentials. Configure X-Fused-End-User-Ref on the MCP client connection for the intended user, then retry."
)

var errMCPEndUserRefRequired = errors.New(mcpEndUserRefRequiredMessage)

type mcpConnectionSelectorsContextKey struct{}

// contextWithMCPConnectionSelectors marks physical calls whose auth selectors came from the MCP connection handshake.
func contextWithMCPConnectionSelectors(ctx context.Context) context.Context {
	return context.WithValue(ctx, mcpConnectionSelectorsContextKey{}, true)
}

// usesMCPConnectionSelectors prevents Unified target selectors and SDK arguments from receiving connection-header guidance.
func usesMCPConnectionSelectors(ctx context.Context) bool {
	value, _ := ctx.Value(mcpConnectionSelectorsContextKey{}).(bool)
	return value
}

// classifyMCPEndUserRefRequirement replaces only a proven pre-provider auth miss that a connection selector can satisfy.
func classifyMCPEndUserRefRequirement(ctx context.Context, identity auth.RuntimeIdentity, auths fusedobject.AuthConfigs, requirements authrouting.Requirements, credentials map[string]any, dispatchErr error) error {
	// Only a dynamic MCP identity on the direct physical bridge sources its user selector from X-Fused-End-User-Ref.
	if !usesMCPConnectionSelectors(ctx) || identity.Kind != store.AppKindMCP || identity.BindingMode != store.AppTokenBindingDynamic {
		return dispatchErr
	}
	var routingErr *engine.AuthRoutingError
	// Other failures may have reached the provider or need a different correction, so their original identity is preserved.
	if !errors.As(dispatchErr, &routingErr) || routingErr.Code != "unsatisfied" {
		return dispatchErr
	}
	// A supplied user or fixed connection selector proves the failure is not caused by the missing MCP header.
	if connectedEndUserRef(credentials) != "" || credentialString(credentials, "fused_connection_id") != "" {
		return dispatchErr
	}
	// The dedicated error is valid only when the operation has a selector-compatible connected-auth route.
	if !hasMatchingConnectedAuthRoute(auths, requirements, credentials) {
		return dispatchErr
	}
	return errMCPEndUserRefRequired
}

// hasMatchingConnectedAuthRoute checks operation-local alternatives without loading credentials or provider data.
func hasMatchingConnectedAuthRoute(auths fusedobject.AuthConfigs, requirements authrouting.Requirements, credentials map[string]any) bool {
	definitions, err := fusedAuthDefinitions(auths)
	// Invalid contracts remain owned by the canonical auth router instead of borrowing user-correction guidance.
	if err != nil {
		return false
	}
	for _, alternative := range requirements {
		// Explicit auth type/name selection excludes unrelated alternatives from the recovery decision.
		if !alternativeMatchesSelectors(alternative, definitions, credentials) {
			continue
		}
		for _, requirement := range alternative.Schemes {
			authConfig, exists := definitions[requirement.Scheme]
			// Unknown schemes cannot establish that adding an end-user selector would satisfy routing.
			if !exists {
				continue
			}
			// OAuth and OIDC are the only auth families backed by Fused connected-user grants.
			if isConnectedAuthSelector(canonicalFusedAuthType(authConfig)) {
				return true
			}
		}
	}
	return false
}
