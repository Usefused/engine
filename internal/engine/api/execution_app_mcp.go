package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// UnifiedAppInputSchema exposes the single authored input contract for an exact hosted MCP Unified App.
func (s *EngineGRPCServer) UnifiedAppInputSchema(ctx context.Context, identity auth.RuntimeIdentity) (json.RawMessage, bool, error) {
	// Direct adapter calls cannot widen the modern route's immutable SDK authority.
	if !admittedUnifiedAppMCPIdentity(s, identity) {
		return nil, false, sandbox.ErrMCPCapabilityUnavailable
	}
	// An old pinned MCP version cannot advertise authored execution after promotion.
	if s.admitUnifiedAppTraffic(ctx, identity.AppID) != nil {
		return nil, false, sandbox.ErrMCPCapabilityUnavailable
	}
	_, manifest, found, requestErr := s.findUnifiedAppBundle(ctx, identity.AppID)
	if requestErr != nil {
		return nil, false, errors.New("unified app bundle is unavailable")
	}
	// A version without attached authored code retains selected raw MCP tools.
	if !found {
		return nil, false, nil
	}
	return manifest.InputSchema, true, nil
}

// ExecuteUnifiedApp invokes the shared durable app command and returns its public execution envelope.
func (s *EngineGRPCServer) ExecuteUnifiedApp(ctx context.Context, identity auth.RuntimeIdentity, input json.RawMessage) (json.RawMessage, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.execute")
	defer span.End()
	span.SetAttributes(attribute.String("execution.trigger", "mcp"), attribute.String("execution.mode", "live"), attribute.String("app.id", identity.AppID.String()))
	// Only an exact runnable Unified App opted into hosted MCP may enter the shared command.
	if !admittedUnifiedAppMCPIdentity(s, identity) {
		span.SetStatus(codes.Error, "mcp_unified_app_denied")
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	// A session pinned before promotion must recheck the family target at each invocation.
	if s.admitUnifiedAppTraffic(ctx, identity.AppID) != nil {
		span.SetStatus(codes.Error, "mcp_unified_app_not_current")
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	bundle, manifest, found, requestErr := s.findUnifiedAppBundle(ctx, identity.AppID)
	// Missing or invalid authored code cannot fall through to the raw MCP child runtime.
	if requestErr != nil || !found {
		span.SetStatus(codes.Error, "mcp_unified_app_unavailable")
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	result, requestErr := s.executeCapabilityRun(ctx, capabilityRunSpec{transport: "mcp",
		identity: identity, version: identity.AppVersion, bundle: bundle,
		manifest: manifest, input: input, mode: "live",
	})
	// A preflight or storage failure has no public execution envelope to return.
	if requestErr != nil {
		span.SetStatus(codes.Error, "mcp_unified_app_runtime_unavailable")
		return nil, errors.New("unified app runtime is unavailable")
	}
	span.SetAttributes(attribute.String("execution.id", result.ExecutionID.String()), attribute.String("execution.status", result.Status))
	// The same typed output and metadata projection is used by the REST response.
	return json.Marshal(result)
}

// admittedUnifiedAppMCPIdentity checks the exact Unified App token scope before reading authored code.
func admittedUnifiedAppMCPIdentity(s *EngineGRPCServer, identity auth.RuntimeIdentity) bool {
	return s != nil && s.store != nil && identity.Kind == store.AppKindUnifiedApp && identity.HostedMCP && identity.Status.Runnable()
}
