package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errUnifiedSchemaInvalid = errors.New("Unified schema is invalid")

type validatedUnifiedRequest struct {
	operation      string
	targets        []string
	input          any
	idempotencyKey string
}

// validateUnifiedRequest rejects malformed unified request before it can cross the Unified preflight admission boundary.
func validateUnifiedRequest(scope *store.AppRuntime, request *enginev1.ExecuteUnifiedRequest) (validatedUnifiedRequest, error) {
	if scope == nil || (scope.Kind != "" && scope.Kind != store.AppKindSDK) {
		return validatedUnifiedRequest{}, status.Error(codes.FailedPrecondition, "Unified execution requires an SDK app")
	}
	if request == nil {
		return validatedUnifiedRequest{}, status.Error(codes.InvalidArgument, "Unified request is required")
	}
	operation, err := validateUnifiedName(request.GetOperation(), maxUnifiedNameBytes)
	if err != nil {
		return validatedUnifiedRequest{}, status.Error(codes.InvalidArgument, "Unified operation name is invalid")
	}
	targets, err := validateUnifiedTargets(request.GetTargets())
	if err != nil {
		return validatedUnifiedRequest{}, err
	}
	if err := validateUnifiedSelectors(request.GetTargetSelectors()); err != nil {
		return validatedUnifiedRequest{}, err
	}
	idempotencyKey, err := validateUnifiedName(request.GetIdempotencyKey(), maxUnifiedSelector)
	if err != nil {
		return validatedUnifiedRequest{}, status.Error(codes.InvalidArgument, "Unified idempotency key is invalid")
	}
	_, input, err := decodeCanonicalUnifiedValue(request.GetInputJson())
	if err != nil {
		return validatedUnifiedRequest{}, status.Error(codes.InvalidArgument, "Unified input must be one bounded JSON document")
	}
	return validatedUnifiedRequest{operation: operation, targets: targets, input: input, idempotencyKey: idempotencyKey}, nil
}

// validateUnifiedName rejects malformed unified name before it can cross the Unified preflight admission boundary.
func validateUnifiedName(value string, maxBytes int) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes {
		return "", errors.New("invalid bounded name")
	}
	return value, nil
}

// validateUnifiedTargets rejects malformed unified targets before it can cross the Unified preflight admission boundary.
func validateUnifiedTargets(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > maxUnifiedTargets {
		return nil, status.Error(codes.InvalidArgument, "Unified targets count is invalid")
	}
	targets := append([]string(nil), values...)
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if _, err := validateUnifiedName(target, maxUnifiedTargetBytes); err != nil {
			return nil, status.Error(codes.InvalidArgument, "Unified target is invalid")
		}
		if _, exists := seen[target]; exists {
			return nil, status.Error(codes.InvalidArgument, "Unified targets must be unique")
		}
		seen[target] = struct{}{}
	}
	return targets, nil
}

// validateUnifiedSelectors rejects malformed selector names and values before
// immutable definitions reveal the allowed service namespaces.
func validateUnifiedSelectors(selectors map[string]*enginev1.ExecutionSelectors) error {
	for target, selector := range selectors {
		// Selector keys use the same bounded public vocabulary as service targets.
		if _, err := validateUnifiedName(target, maxUnifiedTargetBytes); err != nil {
			return status.Error(codes.InvalidArgument, "Unified selector target is invalid")
		}
		if err := validateUnifiedSelector(selector); err != nil {
			return err
		}
	}
	return nil
}

// validateSelectedUnifiedSelectorTargets ensures callers can route only the
// services represented by the selected graph steps.
func validateSelectedUnifiedSelectorTargets(bindings []unified.BindingDefinition, selectors map[string]*enginev1.ExecutionSelectors) error {
	allowed := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		allowed[effectiveUnifiedServiceTarget(binding)] = struct{}{}
	}
	for target := range selectors {
		// Aliases are result namespaces, so they cannot silently become selector namespaces.
		if _, ok := allowed[target]; !ok {
			return status.Error(codes.InvalidArgument, "Unified selector target was not selected")
		}
	}
	return nil
}

// effectiveUnifiedServiceTarget applies the contract default that a binding
// without an explicit service uses its public target.
func effectiveUnifiedServiceTarget(binding unified.BindingDefinition) string {
	// Omitted service targets use the public target by contract.
	if binding.ServiceTarget == "" {
		return binding.PublicTarget
	}
	return binding.ServiceTarget
}

// validateUnifiedSelector rejects malformed unified selector before it can cross the Unified preflight admission boundary.
func validateUnifiedSelector(selector *enginev1.ExecutionSelectors) error {
	if selector == nil {
		return nil
	}
	values := []string{selector.GetEnvironment(), selector.GetEndUserRef(), selector.GetAuthName()}
	for _, value := range values {
		if len(value) > maxUnifiedSelector || value != strings.TrimSpace(value) {
			return status.Error(codes.InvalidArgument, "Unified selector value is invalid")
		}
	}
	if !validUnifiedAuthType(selector.GetAuthType()) {
		return status.Error(codes.InvalidArgument, "Unified auth selector is invalid")
	}
	resourceID := selector.GetResourceId()
	if len(resourceID) > maxUnifiedSelector || resourceID != strings.TrimSpace(resourceID) {
		return status.Error(codes.InvalidArgument, "Unified resource selector is invalid")
	}
	if resourceID != "" {
		if _, err := uuid.Parse(resourceID); err != nil {
			return status.Error(codes.InvalidArgument, "Unified resource selector must be a UUID")
		}
	}
	return nil
}

// validUnifiedAuthType recognizes only the selector vocabulary supported by the connected-auth resolver.
func validUnifiedAuthType(value string) bool {
	switch value {
	case "", "api_key", "oauth", "oidc", "basic", "bearer", "mtls":
		return true
	default:
		return false
	}
}

// loadUnifiedDefinition resolves unified definition from immutable app scope before provider dispatch.
func loadUnifiedDefinition(scope *store.AppRuntime, operation string) (unified.OperationDefinition, error) {
	definition, found, err := lookupUnifiedDefinition(scope, operation)
	if err != nil {
		return unified.OperationDefinition{}, err
	}
	if found {
		return definition, nil
	}
	return unified.OperationDefinition{}, status.Error(codes.InvalidArgument, "Unified operation is not defined for this SDK version")
}

// lookupUnifiedDefinition distinguishes an absent exact name from corrupted
// immutable definition state so REST classification can fail closed.
func lookupUnifiedDefinition(scope *store.AppRuntime, operation string) (unified.OperationDefinition, bool, error) {
	if scope == nil || len(scope.UnifiedDefinitions) == 0 {
		return unified.OperationDefinition{}, false, nil
	}
	if scope.UnifiedDefinitionSchemaVersion != unified.DefinitionSchemaVersion {
		return unified.OperationDefinition{}, false, status.Error(codes.FailedPrecondition, "Unified definitions are unavailable")
	}
	hash, err := unifiedCanonicalHash(scope.UnifiedDefinitions)
	if err != nil || hash != scope.UnifiedDefinitionHash {
		return unified.OperationDefinition{}, false, status.Error(codes.FailedPrecondition, "Unified definitions failed integrity validation")
	}
	definitions, err := unified.DecodeDefinitions(scope.UnifiedDefinitions, unified.DefaultLimits())
	if err != nil {
		return unified.OperationDefinition{}, false, status.Error(codes.FailedPrecondition, "Unified definitions are invalid")
	}
	for _, definition := range definitions {
		if definition.Name == operation {
			return definition, true, nil
		}
	}
	return unified.OperationDefinition{}, false, nil
}

// selectUnifiedBindings narrows immutable Unified preflight admission state to selected targets without adding hidden work.
func selectUnifiedBindings(definition unified.OperationDefinition, targets []string, identity auth.RuntimeIdentity) ([]unified.BindingDefinition, error) {
	byTarget := make(map[string]unified.BindingDefinition, len(definition.Bindings))
	for _, binding := range definition.Bindings {
		byTarget[binding.PublicTarget] = binding
	}
	selected := make([]unified.BindingDefinition, len(targets))
	for index, target := range targets {
		binding, ok := byTarget[target]
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "Unified target is not bound to this operation")
		}
		if !identity.AllowsOperation(binding.OperationID) {
			return nil, status.Error(codes.PermissionDenied, "token does not allow a selected provider operation")
		}
		selected[index] = binding
	}
	if err := validateSelectedUnifiedDependencies(selected); err != nil {
		return nil, err
	}
	activeRollbacks := activeUnifiedRollbackTargets(selected)
	for index, binding := range selected {
		if _, active := activeRollbacks[binding.PublicTarget]; !active {
			// A rollback with no selected direct consumer is inert and cannot
			// impose authorization, resolution, or selector requirements on this call.
			selected[index].Rollback = nil
			continue
		}
		// Active compensation is part of the admitted execution plan, so the
		// wrapper cannot gain authority its physical token lacks.
		if binding.Rollback != nil && !identity.AllowsOperation(binding.Rollback.OperationID) {
			return nil, status.Error(codes.PermissionDenied, "token does not allow a selected rollback operation")
		}
	}
	return selected, nil
}

// activeUnifiedRollbackTargets returns exact dependencies that a selected
// consumer could ask Engine to compensate on failure.
func activeUnifiedRollbackTargets(bindings []unified.BindingDefinition) map[string]struct{} {
	active := make(map[string]struct{})
	for _, binding := range bindings {
		for _, dependency := range binding.DependsOn {
			active[dependency] = struct{}{}
		}
	}
	return active
}

// validateSelectedUnifiedDependencies requires callers to select the complete
// direct dependency set rather than silently adding hidden provider calls.
func validateSelectedUnifiedDependencies(bindings []unified.BindingDefinition) error {
	selected := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		selected[binding.PublicTarget] = struct{}{}
	}
	for _, binding := range bindings {
		for _, dependency := range binding.DependsOn {
			if _, ok := selected[dependency]; !ok {
				return status.Error(codes.InvalidArgument, "Unified selected targets are missing a dependency")
			}
		}
	}
	return nil
}

// exactUnifiedBindings flattens forwards and optional rollbacks into one
// set-resolved, preauthorized physical operation batch.
func exactUnifiedBindings(bindings []unified.BindingDefinition) []sandbox.ExactOperationBinding {
	exact := make([]sandbox.ExactOperationBinding, 0, len(bindings)*2)
	for _, binding := range bindings {
		exact = append(exact, sandbox.ExactOperationBinding{
			ServiceID: binding.ServiceID, ServiceVersionID: binding.ServiceVersionID,
			EndpointID: binding.EndpointID, EndpointName: binding.OperationID,
		})
		if binding.Rollback != nil {
			exact = append(exact, sandbox.ExactOperationBinding{
				ServiceID: binding.Rollback.ServiceID, ServiceVersionID: binding.Rollback.ServiceVersionID,
				EndpointID: binding.Rollback.EndpointID, EndpointName: binding.Rollback.OperationID,
			})
		}
	}
	return exact
}

type resolvedUnifiedBinding struct {
	definition unified.BindingDefinition
	operation  sandbox.ResolvedPhysicalOperation
	rollback   *sandbox.ResolvedPhysicalOperation
}

// alignResolvedUnifiedBindings restores flattened physical operations to their
// containing binding while preserving request order.
func alignResolvedUnifiedBindings(bindings []unified.BindingDefinition, operations []sandbox.ResolvedPhysicalOperation) ([]resolvedUnifiedBinding, error) {
	want := len(exactUnifiedBindings(bindings))
	if len(operations) != want {
		return nil, status.Error(codes.FailedPrecondition, "Unified operation resolution was incomplete")
	}
	resolved := make([]resolvedUnifiedBinding, len(bindings))
	operationIndex := 0
	for index, binding := range bindings {
		resolved[index] = resolvedUnifiedBinding{definition: binding, operation: operations[operationIndex]}
		operationIndex++
		if binding.Rollback != nil {
			rollback := operations[operationIndex]
			resolved[index].rollback = &rollback
			operationIndex++
		}
	}
	return resolved, nil
}

// validateResolvedUnifiedSelectors validates every forward and rollback
// selector contract before the first provider side effect.
func validateResolvedUnifiedSelectors(runtime unifiedPhysicalRuntime, bindings []resolvedUnifiedBinding, selectors map[string]*enginev1.ExecutionSelectors) error {
	for _, binding := range bindings {
		selector := physicalUnifiedSelectors(selectors[effectiveUnifiedServiceTarget(binding.definition)])
		if err := validateOneResolvedUnifiedSelector(runtime, binding.operation, selector); err != nil {
			return err
		}
		if binding.rollback != nil {
			if err := validateOneResolvedUnifiedSelector(runtime, *binding.rollback, selector); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateOneResolvedUnifiedSelector translates physical selector admission
// into the bounded Unified RPC contract.
func validateOneResolvedUnifiedSelector(runtime unifiedPhysicalRuntime, operation sandbox.ResolvedPhysicalOperation, selector sandbox.PhysicalExecutionSelectors) error {
	if err := runtime.ValidateResolvedPhysicalSelectors(operation, selector); err != nil {
		if errors.Is(err, sandbox.ErrPhysicalSelectorContract) {
			return status.Error(codes.InvalidArgument, "Unified selector is incompatible with a selected operation")
		}
		return status.Error(codes.FailedPrecondition, "Unified operation selector contract is unavailable")
	}
	return nil
}

// physicalUnifiedSelectors copies public selector fields into the private physical preflight contract.
func physicalUnifiedSelectors(selector *enginev1.ExecutionSelectors) sandbox.PhysicalExecutionSelectors {
	if selector == nil {
		return sandbox.PhysicalExecutionSelectors{}
	}
	return sandbox.PhysicalExecutionSelectors{
		Environment: selector.GetEnvironment(), EndUserRef: selector.GetEndUserRef(),
		AuthType: selector.GetAuthType(), AuthName: selector.GetAuthName(), ResourceID: selector.GetResourceId(),
	}
}

// prepareUnifiedTargets compiles output schemas and captures only prevalidated
// execution metadata; dependency inputs are evaluated after prerequisites run.
func prepareUnifiedTargets(definition unified.OperationDefinition, bindings []resolvedUnifiedBinding, selectors map[string]*enginev1.ExecutionSelectors) ([]preparedUnifiedTarget, error) {
	targets := make([]preparedUnifiedTarget, len(bindings))
	for index, resolved := range bindings {
		binding := resolved.definition
		output, err := prepareUnifiedOutput(definition.Output, binding.Output)
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, "Unified output definition is invalid")
		}
		targets[index] = preparedUnifiedTarget{
			name: binding.PublicTarget, dependsOn: append([]string(nil), binding.DependsOn...),
			operation: resolved.operation, input: binding.Input,
			selector: selectors[effectiveUnifiedServiceTarget(binding)], output: output,
		}
		if binding.Rollback != nil && resolved.rollback != nil {
			targets[index].rollback = &preparedUnifiedRollback{operation: *resolved.rollback, input: binding.Rollback.Input}
		}
	}
	return targets, nil
}

// evaluateUnifiedInput maps one forward or rollback input against only the
// response namespaces admitted by its compiled program.
func evaluateUnifiedInput(program *unified.Program, input any, target string, responses map[string]any) (map[string]any, []byte, error) {
	mapped := input
	var err error
	if program != nil {
		mapped, err = program.Evaluate(unified.EvaluationContext{Input: input, Target: target, Responses: responses})
	}
	if err != nil || unified.IsOmitted(mapped) {
		return nil, nil, errors.New("provider input mapping failed")
	}
	canonical, err := encodeCanonicalUnifiedValue(mapped)
	if err != nil {
		return nil, nil, err
	}
	_, decoded, err := decodeCanonicalUnifiedValue(canonical)
	if err != nil {
		return nil, nil, err
	}
	params, ok := decoded.(map[string]any)
	if !ok {
		return nil, nil, errors.New("provider input mapping must produce an object")
	}
	return params, canonical, nil
}

// prepareUnifiedOutput assembles unified output without starting persistence, accounting, or provider work.
func prepareUnifiedOutput(root, binding *unified.OutputDefinition) (*preparedUnifiedOutput, error) {
	definition := root
	if definition == nil {
		definition = binding
	}
	if definition == nil {
		return nil, nil
	}
	schema, err := compileUnifiedSchema(definition.Schema)
	if err != nil || definition.Mapping == nil {
		return nil, errUnifiedSchemaInvalid
	}
	return &preparedUnifiedOutput{program: definition.Mapping, schema: schema}, nil
}

// unifiedSelectorCredentials builds the reserved routing envelope consumed by connected-auth resolution.
func unifiedSelectorCredentials(selector *enginev1.ExecutionSelectors) map[string]any {
	credentials := make(map[string]any)
	if selector == nil {
		return credentials
	}
	addUnifiedCredential(credentials, "fused_end_user_ref", selector.GetEndUserRef())
	addUnifiedCredential(credentials, "fused_auth_type", selector.GetAuthType())
	addUnifiedCredential(credentials, "fused_auth_name", selector.GetAuthName())
	addUnifiedCredential(credentials, "fused_resource_id", selector.GetResourceId())
	return credentials
}

// addUnifiedCredential adds one nonempty routing selector to the private credential envelope.
func addUnifiedCredential(credentials map[string]any, key, value string) {
	if value != "" {
		credentials[key] = value
	}
}

// unifiedSelectorEnvironment normalizes the optional environment before physical selector validation.
func unifiedSelectorEnvironment(selector *enginev1.ExecutionSelectors) string {
	if selector == nil {
		return ""
	}
	return selector.GetEnvironment()
}

// deriveUnifiedChildIdentity gives each forward or rollback a distinct stable
// physical idempotency key and binds replay to params plus selectors.
func deriveUnifiedChildIdentity(appID uuid.UUID, operation, target, phase, parentKey string, canonicalParams []byte, selector *enginev1.ExecutionSelectors) (string, string) {
	identity, _ := json.Marshal([]string{appID.String(), operation, target, phase, parentKey})
	idempotencyDigest := sha256.Sum256(identity)
	requestDigest := sha256.Sum256(unifiedChildRequestIdentity(canonicalParams, selector))
	return "unified_" + hex.EncodeToString(idempotencyDigest[:]), hex.EncodeToString(requestDigest[:])
}

// unifiedChildRequestIdentity hashes canonical params and selectors into a phase-specific replay identity.
func unifiedChildRequestIdentity(canonicalParams []byte, selector *enginev1.ExecutionSelectors) []byte {
	// Selectors can change the provider tenant, auth slot, or environment.
	// Binding them to replay identity prevents a reused parent key from serving
	// a response obtained through different routing inputs.
	identity := struct {
		Params      json.RawMessage `json:"params"`
		Environment string          `json:"environment"`
		EndUserRef  string          `json:"end_user_ref"`
		AuthType    string          `json:"auth_type"`
		AuthName    string          `json:"auth_name"`
		ResourceID  string          `json:"resource_id"`
	}{Params: canonicalParams}
	if selector != nil {
		identity.Environment = selector.GetEnvironment()
		identity.EndUserRef = selector.GetEndUserRef()
		identity.AuthType = selector.GetAuthType()
		identity.AuthName = selector.GetAuthName()
		identity.ResourceID = selector.GetResourceId()
	}
	encoded, _ := json.Marshal(identity)
	return encoded
}

// decodeCanonicalUnifiedValue restores canonical unified value only after strict shape, limit, and namespace checks.
func decodeCanonicalUnifiedValue(raw []byte) ([]byte, any, error) {
	canonical, err := canonicaljson.Canonicalize(raw)
	if err != nil {
		return nil, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, err
	}
	return canonical, value, nil
}

// encodeCanonicalUnifiedValue serializes canonical unified value into canonical private bytes for stable hashing.
func encodeCanonicalUnifiedValue(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonicaljson.Canonicalize(raw)
}

// compileUnifiedSchema parses one canonical JSON Schema and rejects external
// references so runtime validation cannot trigger network or file access.
func compileUnifiedSchema(raw json.RawMessage) (*openapi3.Schema, error) {
	canonical, err := canonicaljson.Canonicalize(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding", errUnifiedSchemaInvalid)
	}
	var schema openapi3.Schema
	if err := json.Unmarshal(canonical, &schema); err != nil || schema.Type == nil {
		return nil, fmt.Errorf("%w: shape", errUnifiedSchemaInvalid)
	}
	if err := schema.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("%w: contract", errUnifiedSchemaInvalid)
	}
	return &schema, nil
}

// validateUnifiedValue rejects malformed unified value before it can cross the Unified preflight admission boundary.
func validateUnifiedValue(raw json.RawMessage, value any) error {
	schema, err := compileUnifiedSchema(raw)
	if err != nil {
		return err
	}
	return schema.VisitJSON(value)
}

// unifiedInputSchemaError maps schema library details to one stable invalid-argument response.
func unifiedInputSchemaError(err error) error {
	if errors.Is(err, errUnifiedSchemaInvalid) {
		return status.Error(codes.FailedPrecondition, "Unified input definition is invalid")
	}
	return status.Error(codes.InvalidArgument, "Unified input failed schema validation")
}
