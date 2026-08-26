package api

import (
	"context"
	"time"

	"github.com/Usefused/engine/internal/engine"
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
	receiptID      uuid.UUID
	identity       auth.RuntimeIdentity
	appID          uuid.UUID
	operation      string
	idempotencyKey string
	transport      string
	input          any
	targets        []preparedUnifiedTarget
	output         *preparedUnifiedOutput
}

type preparedUnifiedTarget struct {
	name       string
	dependsOn  []string
	operation  sandbox.ResolvedPhysicalOperation
	input      *unified.Program
	selector   *enginev1.ExecutionSelectors
	pagination *engine.PaginationIntent
	output     *preparedUnifiedOutput
	rollback   *preparedUnifiedRollback
}

// preparedUnifiedRollback contains one exact, preauthorized compensation and
// its self-response mapping.
type preparedUnifiedRollback struct {
	operation sandbox.ResolvedPhysicalOperation
	input     *unified.Program
}

// preparedUnifiedOutput pairs one immutable projection program with the
// schema that validates its evaluated JSON value before it becomes public.
type preparedUnifiedOutput struct {
	program *unified.Program
	schema  *openapi3.Schema
}

// ExecuteUnified adds a logical receipt while each selected provider still enters
// the established physical boundary that owns authorization, retries and usage.
func (s *EngineGRPCServer) ExecuteUnified(ctx context.Context, request *enginev1.ExecuteUnifiedRequest) (response *enginev1.ExecuteUnifiedResponse, err error) {
	started := time.Now()
	transport := sandbox.ExecutionTransportFromContext(ctx)
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified.execute")
	span.SetAttributes(
		attribute.String("execution.transport", transport),
		attribute.Int("unified.target_count", boundedUnifiedTargetCount(request)),
	)
	stage := "authentication"
	defer func() { finishUnifiedSpan(span, stage, response, err) }()

	scope, identity, err := s.authenticatedAppRuntimeFromGRPC(ctx)
	// Unauthenticated callers cannot manufacture app history.
	if err != nil {
		return nil, err
	}
	stage = "validation"
	call, err := s.prepareUnifiedCall(ctx, scope, identity, request, transport)
	// Rejected definitions/selectors stay pre-dispatch and have no logical execution.
	if err != nil {
		return nil, err
	}
	stage = "dispatch"
	return s.executePreparedUnified(ctx, call, started), nil
}

// boundedUnifiedTargetCount caps target-derived span metadata before any caller value reaches telemetry.
func boundedUnifiedTargetCount(request *enginev1.ExecuteUnifiedRequest) int {
	if request == nil {
		return 0
	}
	return min(len(request.GetTargets()), maxUnifiedTargets+1)
}

// prepareUnifiedCall authenticates, validates, resolves, and preflights the complete logical call before scheduling.
func (s *EngineGRPCServer) prepareUnifiedCall(ctx context.Context, scope *store.AppRuntime, identity auth.RuntimeIdentity, request *enginev1.ExecuteUnifiedRequest, transport string) (preparedUnifiedCall, error) {
	validated, err := validateUnifiedRequest(scope, request, transport)
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
	targets, err := prepareResolvedUnifiedTargets(resolved, request.GetTargetSelectors(), validated.pagination)
	if err != nil {
		return preparedUnifiedCall{}, err
	}
	output, err := prepareUnifiedOperationOutput(definition.Output)
	if err != nil {
		return preparedUnifiedCall{}, err
	}
	return preparedUnifiedCall{
		identity: identity, appID: scope.AppID, operation: definition.Name,
		idempotencyKey: validated.idempotencyKey, transport: transport,
		input: validated.input, targets: targets, output: output,
	}, nil
}

// prepareResolvedUnifiedTargets applies request-scoped controls before compiling immutable binding programs.
func prepareResolvedUnifiedTargets(resolved []resolvedUnifiedBinding, selectors map[string]*enginev1.ExecutionSelectors, pagination map[string]*engine.PaginationIntent) ([]preparedUnifiedTarget, error) {
	if err := validateResolvedUnifiedPaginationIntents(resolved, pagination); err != nil {
		return nil, err
	}
	return prepareUnifiedTargets(resolved, selectors, pagination)
}

// prepareUnifiedOperationOutput translates immutable output corruption into one stable pre-dispatch error.
func prepareUnifiedOperationOutput(definition *unified.OutputDefinition) (*preparedUnifiedOutput, error) {
	output, err := prepareUnifiedOutput(definition)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "Unified output definition is invalid")
	}
	return output, nil
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
	outputCode := response.GetOutputErrorCode()
	if outputCode != "" {
		outcome = "failed"
		attributes = append(attributes, attribute.String("unified.output_error_code", outputCode))
		span.SetStatus(otelcodes.Error, outputCode)
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
