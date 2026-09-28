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

// ExecutionAppInputSchema exposes the single authored input contract for an exact hosted MCP Execution App.
func (s *EngineGRPCServer) ExecutionAppInputSchema(ctx context.Context, identity auth.RuntimeIdentity) (json.RawMessage, bool, error) {
	// Direct adapter calls cannot widen the modern route's immutable SDK authority.
	if !admittedExecutionAppMCPIdentity(s, identity) {
		return nil, false, sandbox.ErrMCPCapabilityUnavailable
	}
	// An old pinned MCP version cannot advertise authored execution after promotion.
	if s.admitExecutionAppTraffic(ctx, identity.AppID) != nil {
		return nil, false, sandbox.ErrMCPCapabilityUnavailable
	}
	_, manifest, found, requestErr := s.findExecutionAppBundle(ctx, identity.AppID)
	if requestErr != nil {
		return nil, false, errors.New("execution app bundle is unavailable")
	}
	// A version without attached authored code retains selected raw MCP tools.
	if !found {
		return nil, false, nil
	}
	return manifest.InputSchema, true, nil
}

// ExecuteExecutionApp invokes the shared durable app command and returns its public execution envelope.
func (s *EngineGRPCServer) ExecuteExecutionApp(ctx context.Context, identity auth.RuntimeIdentity, input json.RawMessage) (json.RawMessage, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_app.execute")
	defer span.End()
	span.SetAttributes(attribute.String("execution.trigger", "mcp"), attribute.String("execution.mode", "live"), attribute.String("app.id", identity.AppID.String()))
	// Only an exact runnable Execution App opted into hosted MCP may enter the shared command.
	if !admittedExecutionAppMCPIdentity(s, identity) {
		span.SetStatus(codes.Error, "mcp_execution_app_denied")
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	// A session pinned before promotion must recheck the family target at each invocation.
	if s.admitExecutionAppTraffic(ctx, identity.AppID) != nil {
		span.SetStatus(codes.Error, "mcp_execution_app_not_current")
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	bundle, manifest, found, requestErr := s.findExecutionAppBundle(ctx, identity.AppID)
	// Missing or invalid authored code cannot fall through to the raw MCP child runtime.
	if requestErr != nil || !found {
		span.SetStatus(codes.Error, "mcp_execution_app_unavailable")
		return nil, sandbox.ErrMCPCapabilityUnavailable
	}
	result, requestErr := s.executeCapabilityRun(ctx, capabilityRunSpec{
		identity: identity, version: identity.AppVersion, bundle: bundle,
		manifest: manifest, input: input, mode: "live",
	})
	// A preflight or storage failure has no public execution envelope to return.
	if requestErr != nil {
		span.SetStatus(codes.Error, "mcp_execution_app_runtime_unavailable")
		return nil, errors.New("execution app runtime is unavailable")
	}
	span.SetAttributes(attribute.String("execution.id", result.ExecutionID.String()), attribute.String("execution.status", result.Status))
	// The same typed output and metadata projection is used by the REST response.
	return json.Marshal(result)
}

// admittedExecutionAppMCPIdentity checks the exact Execution App token scope before reading authored code.
func admittedExecutionAppMCPIdentity(s *EngineGRPCServer, identity auth.RuntimeIdentity) bool {
	return s != nil && s.store != nil && identity.Kind == store.AppKindExecution && identity.HostedMCP && identity.Status.Runnable()
}
