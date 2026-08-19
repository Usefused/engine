package api

import (
	"context"

	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxUnifiedTargets     = 16
	maxUnifiedConcurrency = 4
	maxUnifiedNameBytes   = 256
	maxUnifiedTargetBytes = 253
	maxUnifiedSelector    = 256
)

type unifiedPhysicalRuntime interface {
	ResolveExactPhysicalOperations(context.Context, uuid.UUID, []sandbox.ExactOperationBinding) ([]sandbox.ResolvedPhysicalOperation, error)
	ValidateResolvedPhysicalSelectors(sandbox.ResolvedPhysicalOperation, sandbox.PhysicalExecutionSelectors) error
	ExecuteResolvedPhysicalJSON(context.Context, auth.RuntimeIdentity, sandbox.ResolvedPhysicalOperation, sandbox.PhysicalExecutionRequest) (sandbox.PhysicalExecutionResult, error)
	ExecuteResolvedPhysicalSuccess(context.Context, auth.RuntimeIdentity, sandbox.ResolvedPhysicalOperation, sandbox.PhysicalExecutionRequest) error
}

type preparedUnifiedCall struct {
	identity       auth.RuntimeIdentity
	appID          uuid.UUID
	operation      string
	idempotencyKey string
	input          any
	targets        []preparedUnifiedTarget
}

type preparedUnifiedTarget struct {
	name      string
	dependsOn []string
	operation sandbox.ResolvedPhysicalOperation
	input     *unified.Program
	selector  *enginev1.ExecutionSelectors
	output    *preparedUnifiedOutput
	rollback  *preparedUnifiedRollback
}

// preparedUnifiedRollback contains one exact, preauthorized compensation and
// its self-response mapping.
type preparedUnifiedRollback struct {
	operation sandbox.ResolvedPhysicalOperation
	input     *unified.Program
}

type preparedUnifiedOutput struct {
	program *unified.Program
	schema  *openapi3.Schema
}

// ExecuteUnified is a logical wrapper only. Each selected provider still
// enters the established physical boundary, which remains the single owner of
// authorization, retry, auditing, usage, and provider dispatch behavior.
func (s *EngineGRPCServer) ExecuteUnified(ctx context.Context, request *enginev1.ExecuteUnifiedRequest) (response *enginev1.ExecuteUnifiedResponse, err error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified.execute")
	span.SetAttributes(attribute.Int("unified.target_count", boundedUnifiedTargetCount(request)))
	stage := "authentication"
	defer func() { finishUnifiedSpan(span, stage, response, err) }()

	scope, identity, err := s.authenticatedAppRuntimeFromGRPC(ctx)
	if err != nil {
		return nil, err
	}
	stage = "validation"
	call, err := s.prepareUnifiedCall(ctx, scope, identity, request)
	if err != nil {
		return nil, err
	}
	stage = "dispatch"
	results, rollbacks := s.executeUnifiedGraph(ctx, call)
	return &enginev1.ExecuteUnifiedResponse{Results: results, RollbackResults: rollbacks}, nil
}

// boundedUnifiedTargetCount caps target-derived span metadata before any caller value reaches telemetry.
func boundedUnifiedTargetCount(request *enginev1.ExecuteUnifiedRequest) int {
	if request == nil {
		return 0
	}
	return min(len(request.GetTargets()), maxUnifiedTargets+1)
}

// prepareUnifiedCall authenticates, validates, resolves, and preflights the complete logical call before scheduling.
func (s *EngineGRPCServer) prepareUnifiedCall(ctx context.Context, scope *store.AppRuntime, identity auth.RuntimeIdentity, request *enginev1.ExecuteUnifiedRequest) (preparedUnifiedCall, error) {
	validated, err := validateUnifiedRequest(scope, request)
	if err != nil {
		return preparedUnifiedCall{}, err
	}
	definition, err := loadUnifiedDefinition(scope, validated.operation)
	if err != nil {
		return preparedUnifiedCall{}, err
	}
	bindings, err := admitUnifiedBindings(definition, validated, identity, request.GetTargetSelectors())
	if err != nil {
		return preparedUnifiedCall{}, err
	}
	if s.unifiedRuntime == nil {
		return preparedUnifiedCall{}, status.Error(codes.FailedPrecondition, "Unified execution is unavailable")
	}
	exactBindings := exactUnifiedBindings(bindings)
	operations, err := s.unifiedRuntime.ResolveExactPhysicalOperations(ctx, scope.AppID, exactBindings)
	if err != nil {
		return preparedUnifiedCall{}, status.Error(codes.FailedPrecondition, "Unified operation bindings are unavailable")
	}
	// Selectors are whole-call preconditions. Validate the complete aligned
	// set before mapping or fanout so a later mismatch cannot follow an earlier
	// provider side effect.
	resolved, err := alignResolvedUnifiedBindings(bindings, operations)
	if err != nil {
		return preparedUnifiedCall{}, err
	}
	if err := validateResolvedUnifiedSelectors(s.unifiedRuntime, resolved, request.GetTargetSelectors()); err != nil {
		return preparedUnifiedCall{}, err
	}
	targets, err := prepareUnifiedTargets(definition, resolved, request.GetTargetSelectors())
	if err != nil {
		return preparedUnifiedCall{}, err
	}
	return preparedUnifiedCall{
		identity: identity, appID: scope.AppID, operation: definition.Name,
		idempotencyKey: validated.idempotencyKey, input: validated.input, targets: targets,
	}, nil
}

// admitUnifiedBindings validates logical input, graph authority, and service
// selector namespaces before exact endpoint resolution begins.
func admitUnifiedBindings(definition unified.OperationDefinition, request validatedUnifiedRequest, identity auth.RuntimeIdentity, selectors map[string]*enginev1.ExecutionSelectors) ([]unified.BindingDefinition, error) {
	if err := validateUnifiedValue(definition.InputSchema, request.input); err != nil {
		return nil, unifiedInputSchemaError(err)
	}
	bindings, err := selectUnifiedBindings(definition, request.targets, identity)
	if err != nil {
		return nil, err
	}
	if err := validateSelectedUnifiedSelectorTargets(bindings, selectors); err != nil {
		return nil, err
	}
	return bindings, nil
}

// finishUnifiedSpan closes Unified RPC execution with one bounded outcome and collected physical results.
func finishUnifiedSpan(span trace.Span, stage string, response *enginev1.ExecuteUnifiedResponse, err error) {
	defer span.End()
	attributes := []attribute.KeyValue{
		attribute.Int("unified.schema_version", unified.DefinitionSchemaVersion),
		attribute.String("unified.stage", stage),
	}
	if err != nil {
		code := unifiedRPCErrorCode(err)
		span.SetAttributes(append(attributes, attribute.String("unified.outcome", "rejected"), attribute.String("unified.error_code", code))...)
		span.SetStatus(otelcodes.Error, code)
		return
	}
	counts := countUnifiedResponse(response)
	outcome := "success"
	if counts.errors > 0 || counts.skipped > 0 || counts.rollbackErrors > 0 {
		outcome = "partial"
	}
	span.SetAttributes(append(attributes,
		attribute.String("unified.outcome", outcome),
		attribute.Int("unified.target_count", counts.successes+counts.errors+counts.skipped),
		attribute.Int("unified.success_count", counts.successes),
		attribute.Int("unified.error_count", counts.errors),
		attribute.Int("unified.skipped_count", counts.skipped),
		attribute.Int("unified.rollback_count", counts.rollbackSuccesses+counts.rollbackErrors),
		attribute.Int("unified.rollback_success_count", counts.rollbackSuccesses),
		attribute.Int("unified.rollback_error_count", counts.rollbackErrors),
	)...)
}

type unifiedResponseCounts struct {
	successes         int
	errors            int
	skipped           int
	rollbackSuccesses int
	rollbackErrors    int
}

// countUnifiedResponse returns bounded wrapper-only outcome counts without
// inspecting or recording provider data.
func countUnifiedResponse(response *enginev1.ExecuteUnifiedResponse) unifiedResponseCounts {
	if response == nil {
		return unifiedResponseCounts{}
	}
	counts := unifiedResponseCounts{}
	for _, result := range response.GetResults() {
		switch result.GetStatus() {
		case "success":
			counts.successes++
		case "skipped":
			counts.skipped++
		default:
			counts.errors++
		}
	}
	for _, result := range response.GetRollbackResults() {
		if result.GetStatus() == "success" {
			counts.rollbackSuccesses++
		} else {
			counts.rollbackErrors++
		}
	}
	return counts
}

// unifiedRPCErrorCode reduces gRPC failures to a bounded status label safe for wrapper telemetry.
func unifiedRPCErrorCode(err error) string {
	switch status.Code(err) {
	case codes.Unauthenticated:
		return "authentication_failed"
	case codes.PermissionDenied:
		return "operation_not_allowed"
	case codes.InvalidArgument:
		return "request_invalid"
	case codes.FailedPrecondition:
		return "definition_unavailable"
	default:
		return "execution_rejected"
	}
}
