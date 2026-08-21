package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type sdkUnifiedCompilation struct {
	Definitions           []unified.OperationDefinition
	DefinitionJSON        json.RawMessage
	DefinitionHash        string
	CodegenDescriptorHash string
	Descriptors           *models.SDKUnifiedOperationDescriptors
}

type sdkUnifiedBindingRequest struct {
	operationName string
	target        string
	serviceTarget string
	binding       sdkUnifiedBindingDoc
	selection     models.SDKSelection
	rollback      bool
}

type sdkUnifiedContractStore interface {
	ListServiceContractEndpointsForSelections(context.Context, []store.ServiceContractEndpointSelection, []string) ([]store.ServiceContractEndpointMatch, error)
}

// compileSDKUnifiedOperations resolves every forward and rollback in one batch,
// then produces private definitions and credential-free public descriptors.
func compileSDKUnifiedOperations(
	ctx context.Context,
	s store.Store,
	doc sdkConfigDocument,
	selections []models.SDKSelection,
	services []sdkResolvedService,
) (sdkUnifiedCompilation, error) {
	if len(doc.UnifiedOperations) == 0 {
		return emptySDKUnifiedCompilation(), nil
	}
	contractStore, ok := s.(sdkUnifiedContractStore)
	if !ok {
		return sdkUnifiedCompilation{}, unifiedCompileError("contract_store_unavailable")
	}
	requests, err := sdkUnifiedBindingRequests(doc, selections, services)
	if err != nil {
		return sdkUnifiedCompilation{}, err
	}
	endpoints, err := resolveSDKUnifiedEndpoints(ctx, contractStore, requests)
	if err != nil {
		recordSDKUnifiedCompile(ctx, len(doc.UnifiedOperations), len(requests), "endpoint_resolution_failed")
		return sdkUnifiedCompilation{}, err
	}
	compiled, err := buildSDKUnifiedCompilation(doc, requests, endpoints)
	if err != nil {
		recordSDKUnifiedCompile(ctx, len(doc.UnifiedOperations), len(requests), "definition_invalid")
		return sdkUnifiedCompilation{}, err
	}
	recordSDKUnifiedCompile(ctx, len(compiled.Definitions), len(requests), "success")
	return compiled, nil
}

// emptySDKUnifiedCompilation returns canonical empty-set bytes and hashes for SDKs with no Unified methods.
func emptySDKUnifiedCompilation() sdkUnifiedCompilation {
	return sdkUnifiedCompilation{
		Definitions:           []unified.OperationDefinition{},
		DefinitionJSON:        json.RawMessage("[]"),
		DefinitionHash:        store.EmptyUnifiedSetHash,
		CodegenDescriptorHash: store.EmptyUnifiedSetHash,
	}
}

// sdkUnifiedBindingRequests flattens forward and rollback bindings into one stable exact-resolution request list.
func sdkUnifiedBindingRequests(doc sdkConfigDocument, selections []models.SDKSelection, services []sdkResolvedService) ([]sdkUnifiedBindingRequest, error) {
	if len(selections) != len(services) {
		return nil, unifiedCompileError("selection_mismatch")
	}
	byTarget := make(map[string]models.SDKSelection, len(services))
	for index, service := range services {
		byTarget[service.PublicTarget] = selections[index]
	}
	requests := make([]sdkUnifiedBindingRequest, 0, unifiedPhysicalOperationCount(doc.UnifiedOperations))
	for _, operationName := range sortedUnifiedOperationNames(doc.UnifiedOperations) {
		operation := doc.UnifiedOperations[operationName]
		for _, target := range sortedUnifiedBindingTargets(operation.Bindings) {
			binding := operation.Bindings[target]
			serviceTarget := unifiedBindingServiceTarget(target, binding.Service)
			selection, ok := byTarget[serviceTarget]
			// Compilation fails closed if validated configuration and resolved selections drift.
			if !ok {
				return nil, unifiedCompileError("target_resolution_failed")
			}
			requests = append(requests, sdkUnifiedBindingRequest{
				operationName: operationName, target: target, serviceTarget: serviceTarget,
				binding: binding, selection: selection,
			})
			// Forward and compensation operations share one snapshot query, so
			// every possible physical call is resolved before any app is applied.
			if binding.Rollback != nil {
				requests = append(requests, sdkUnifiedBindingRequest{
					operationName: operationName, target: target, serviceTarget: serviceTarget,
					binding: binding, selection: selection, rollback: true,
				})
			}
		}
	}
	return requests, nil
}

// sortedUnifiedBindingTargets provides deterministic declaration order for compilation, hashing, and result alignment.
func sortedUnifiedBindingTargets(bindings map[string]sdkUnifiedBindingDoc) []string {
	targets := make([]string, 0, len(bindings))
	for target := range bindings {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}

// resolveSDKUnifiedEndpoints resolves sdk unified endpoints from immutable app scope before provider dispatch.
func resolveSDKUnifiedEndpoints(ctx context.Context, contractStore sdkUnifiedContractStore, requests []sdkUnifiedBindingRequest) ([]store.ServiceContractEndpointMatch, error) {
	selections := make([]store.ServiceContractEndpointSelection, len(requests))
	for index, request := range requests {
		selection := request.selection
		selections[index] = store.ServiceContractEndpointSelection{
			SelectionIndex: index, ServiceID: selection.ServiceID,
			ServiceVersionID: selection.ServiceVersionID, SelectAll: selection.SelectAll,
			EndpointIDs: selection.EndpointIDs, OperationNames: selection.OperationNames,
			EndpointNames: []string{request.resolvedOperationID()},
		}
	}
	matches, err := contractStore.ListServiceContractEndpointsForSelections(ctx, selections, nil)
	if err != nil {
		return nil, unifiedCompileError("endpoint_snapshot_unavailable")
	}
	return exactSDKUnifiedEndpointMatches(matches, len(requests))
}

// resolvedOperationID returns the exact operation represented by one flattened
// forward-or-rollback snapshot request.
func (request sdkUnifiedBindingRequest) resolvedOperationID() string {
	if request.rollback {
		return request.binding.Rollback.Operation
	}
	return request.binding.Operation
}

// exactSDKUnifiedEndpointMatches aligns every flattened forward and rollback request with one resolver row and fails on gaps.
func exactSDKUnifiedEndpointMatches(matches []store.ServiceContractEndpointMatch, count int) ([]store.ServiceContractEndpointMatch, error) {
	resolved := make([]store.ServiceContractEndpointMatch, count)
	present := make([]bool, count)
	for _, match := range matches {
		if match.SelectionIndex < 0 || match.SelectionIndex >= count || present[match.SelectionIndex] {
			return nil, unifiedCompileError("endpoint_resolution_ambiguous")
		}
		resolved[match.SelectionIndex], present[match.SelectionIndex] = match, true
	}
	for _, found := range present {
		if !found {
			return nil, unifiedCompileError("endpoint_not_found")
		}
	}
	return resolved, nil
}

// buildSDKUnifiedCompilation assembles private definitions, public descriptors, and their hashes from one resolved endpoint snapshot.
func buildSDKUnifiedCompilation(doc sdkConfigDocument, requests []sdkUnifiedBindingRequest, endpoints []store.ServiceContractEndpointMatch) (sdkUnifiedCompilation, error) {
	byOperation := make(map[string][]int, len(doc.UnifiedOperations))
	rollbackEndpoints := make(map[unifiedRequestKey]store.ServiceContractEndpointMatch)
	for index, request := range requests {
		// Rollback snapshot rows support their containing forward binding but
		// must never become a second public target.
		if request.rollback {
			rollbackEndpoints[request.key()] = endpoints[index]
			continue
		}
		byOperation[request.operationName] = append(byOperation[request.operationName], index)
	}
	definitions := make([]unified.OperationDefinition, 0, len(doc.UnifiedOperations))
	descriptors := make([]models.SDKUnifiedOperationDescriptor, 0, len(doc.UnifiedOperations))
	for _, name := range sortedUnifiedOperationNames(doc.UnifiedOperations) {
		definition, descriptor, err := compileSDKUnifiedOperation(doc.UnifiedOperations[name], name, byOperation[name], requests, endpoints, rollbackEndpoints)
		if err != nil {
			return sdkUnifiedCompilation{}, err
		}
		definitions = append(definitions, definition)
		descriptors = append(descriptors, descriptor)
	}
	public := &models.SDKUnifiedOperationDescriptors{
		SchemaVersion: models.SDKUnifiedDescriptorSchemaVersion,
		Operations:    descriptors,
	}
	return finalizeSDKUnifiedCompilation(definitions, public)
}

type unifiedRequestKey struct {
	operationName string
	target        string
}

// key identifies one binding's optional rollback inside one Unified operation.
func (request sdkUnifiedBindingRequest) key() unifiedRequestKey {
	return unifiedRequestKey{operationName: request.operationName, target: request.target}
}

// compileSDKUnifiedOperation assembles one logical method while keeping private
// mapping programs separate from the Registry-facing descriptor.
func compileSDKUnifiedOperation(
	doc sdkUnifiedOperationDoc,
	name string,
	indices []int,
	requests []sdkUnifiedBindingRequest,
	endpoints []store.ServiceContractEndpointMatch,
	rollbackEndpoints map[unifiedRequestKey]store.ServiceContractEndpointMatch,
) (unified.OperationDefinition, models.SDKUnifiedOperationDescriptor, error) {
	definition := unified.OperationDefinition{
		Name: name, Description: doc.Description, InputSchema: canonicalUnifiedJSON(doc.Input),
		Bindings: make([]unified.BindingDefinition, 0, len(indices)),
	}
	descriptor := models.SDKUnifiedOperationDescriptor{
		Name: name, Description: doc.Description, InputSchema: canonicalUnifiedJSON(doc.Input),
		Targets: make([]models.SDKUnifiedTargetDescriptor, 0, len(indices)),
	}
	allowedTargets := sortedUnifiedBindingTargets(doc.Bindings)
	for _, index := range indices {
		request := requests[index]
		rollbackMatch, hasRollback := rollbackEndpoints[request.key()]
		binding, targetDescriptor, err := compileSDKUnifiedBinding(request, endpoints[index], rollbackMatch, hasRollback)
		if err != nil {
			return unified.OperationDefinition{}, models.SDKUnifiedOperationDescriptor{}, err
		}
		definition.Bindings = append(definition.Bindings, binding)
		descriptor.Targets = append(descriptor.Targets, targetDescriptor)
	}
	output, err := compileSDKUnifiedOutput(doc.Output, allowedTargets)
	if err != nil {
		return unified.OperationDefinition{}, models.SDKUnifiedOperationDescriptor{}, err
	}
	definition.Output = output
	if output != nil {
		descriptor.OutputSchema = append(json.RawMessage(nil), output.Schema...)
	}
	return definition, descriptor, nil
}

// compileSDKUnifiedBinding binds one public target to its exact endpoint and
// admits response dataflow only from declared direct dependencies.
func compileSDKUnifiedBinding(request sdkUnifiedBindingRequest, match, rollbackMatch store.ServiceContractEndpointMatch, hasRollback bool) (unified.BindingDefinition, models.SDKUnifiedTargetDescriptor, error) {
	// Input response access is exactly the declared dependency set, keeping
	// dataflow authority identical to scheduling and rollback authority.
	input, err := compileSDKUnifiedDynamicValue(request.binding.Input, request.binding.DependsOn)
	if err != nil {
		return unified.BindingDefinition{}, models.SDKUnifiedTargetDescriptor{}, unifiedCompileError("binding_input_invalid")
	}
	output, err := compileSDKUnifiedOutput(request.binding.Output, []string{request.target})
	if err != nil {
		return unified.BindingDefinition{}, models.SDKUnifiedTargetDescriptor{}, err
	}
	binding := unified.BindingDefinition{
		PublicTarget: request.target, ServiceTarget: request.serviceTarget,
		OperationID: request.binding.Operation,
		ServiceID:   request.selection.ServiceID, ServiceVersionID: request.selection.ServiceVersionID,
		EndpointID: match.Endpoint.ID, DependsOn: append([]string(nil), request.binding.DependsOn...),
		Input: input, Output: output,
	}
	descriptor := models.SDKUnifiedTargetDescriptor{
		PublicTarget: request.target, ServiceTarget: request.serviceTarget,
		OperationID: request.binding.Operation,
		ServiceID:   request.selection.ServiceID, ServiceVersionID: request.selection.ServiceVersionID,
		EndpointID: match.Endpoint.ID,
		DependsOn:  append([]string(nil), request.binding.DependsOn...),
	}
	rollback, rollbackDescriptor, err := compileSDKUnifiedRollback(request, rollbackMatch, hasRollback)
	if err != nil {
		return unified.BindingDefinition{}, models.SDKUnifiedTargetDescriptor{}, err
	}
	binding.Rollback = rollback
	descriptor.Rollback = rollbackDescriptor
	if output != nil {
		descriptor.OutputSchema = append(json.RawMessage(nil), output.Schema...)
	}
	return binding, descriptor, nil
}

// compileSDKUnifiedRollback binds one optional compensation to its exact
// endpoint and restricts its input mapping to the compensated response.
func compileSDKUnifiedRollback(request sdkUnifiedBindingRequest, match store.ServiceContractEndpointMatch, hasMatch bool) (*unified.RollbackDefinition, *models.SDKUnifiedRollbackDescriptor, error) {
	if request.binding.Rollback == nil {
		return nil, nil, nil
	}
	// Every declared rollback must have participated in the same exact
	// snapshot query as forwards; an absent match fails app planning closed.
	if !hasMatch {
		return nil, nil, unifiedCompileError("rollback_endpoint_not_found")
	}
	input, err := compileSDKUnifiedDynamicValue(request.binding.Rollback.Input, []string{request.target})
	if err != nil {
		return nil, nil, unifiedCompileError("rollback_input_invalid")
	}
	definition := &unified.RollbackDefinition{
		OperationID: request.binding.Rollback.Operation,
		ServiceID:   request.selection.ServiceID, ServiceVersionID: request.selection.ServiceVersionID,
		EndpointID: match.Endpoint.ID, Input: input,
	}
	descriptor := &models.SDKUnifiedRollbackDescriptor{
		OperationID: definition.OperationID, ServiceID: definition.ServiceID,
		ServiceVersionID: definition.ServiceVersionID, EndpointID: definition.EndpointID,
	}
	return definition, descriptor, nil
}

// compileSDKUnifiedOutput pairs one canonical schema with its private mapping,
// or leaves output provider-specific when no root projection is declared.
func compileSDKUnifiedOutput(output json.RawMessage, allowedTargets []string) (*unified.OutputDefinition, error) {
	if len(output) == 0 {
		return nil, nil
	}
	schema, source, err := compileSDKUnifiedOutputDocument(output, allowedTargets)
	if err != nil {
		return nil, unifiedCompileError("output_definition_invalid")
	}
	mapping, err := compileSDKUnifiedDynamicValue(source.raw, source.allowedTargets)
	if err != nil || mapping == nil {
		return nil, unifiedCompileError("output_mapping_invalid")
	}
	return &unified.OutputDefinition{Schema: schema, Mapping: mapping}, nil
}

// compileSDKUnifiedDynamicValue converts strict JSON into bounded bytecode and
// restricts response references to the caller-supplied target set.
func compileSDKUnifiedDynamicValue(raw json.RawMessage, allowedTargets []string) (*unified.Program, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	value, err := decodeSDKUnifiedValue(raw)
	if err != nil {
		return nil, err
	}
	// Response references are validated against the operation's exact public
	// targets by the compiler. Runtime therefore evaluates only reviewed names.
	return unified.CompileWithTargets(value, unified.DefaultLimits(), allowedTargets)
}

// decodeSDKUnifiedValue restores sdk unified value only after strict shape, limit, and namespace checks.
func decodeSDKUnifiedValue(raw json.RawMessage) (any, error) {
	canonical, err := canonicaljson.Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// finalizeSDKUnifiedCompilation closes SDK Unified compilation with one bounded outcome and collected physical results.
func finalizeSDKUnifiedCompilation(definitions []unified.OperationDefinition, descriptors *models.SDKUnifiedOperationDescriptors) (sdkUnifiedCompilation, error) {
	definitionJSON, err := unified.EncodeDefinitions(definitions, unified.DefaultLimits())
	if err != nil {
		return sdkUnifiedCompilation{}, unifiedCompileError("definition_encode_failed")
	}
	descriptorJSON, err := json.Marshal(descriptors)
	if err != nil {
		return sdkUnifiedCompilation{}, unifiedCompileError("descriptor_encode_failed")
	}
	definitionHash, err := unifiedCanonicalHash(definitionJSON)
	if err != nil {
		return sdkUnifiedCompilation{}, unifiedCompileError("definition_hash_failed")
	}
	descriptorHash, err := unifiedCanonicalHash(descriptorJSON)
	if err != nil {
		return sdkUnifiedCompilation{}, unifiedCompileError("descriptor_hash_failed")
	}
	return sdkUnifiedCompilation{
		Definitions: definitions, DefinitionJSON: definitionJSON,
		DefinitionHash: definitionHash, CodegenDescriptorHash: descriptorHash,
		Descriptors: descriptors,
	}, nil
}

// unifiedCanonicalHash hashes canonical JSON so immutable app identity does not depend on formatting.
func unifiedCanonicalHash(raw []byte) (string, error) {
	digest, err := canonicaljson.HexSHA256(raw)
	if err != nil {
		return "", err
	}
	return "sha256:" + digest, nil
}

// normalizeAndValidateResolvedUnifiedPayload applies canonical defaults and rechecks immutable SDK Unified compilation hashes before apply.
func normalizeAndValidateResolvedUnifiedPayload(payload *appResolvedPayload) error {
	if payload.UnifiedDefinitionSchemaVersion == 0 {
		payload.UnifiedDefinitionSchemaVersion = unified.DefinitionSchemaVersion
	}
	if len(payload.UnifiedDefinitions) == 0 {
		payload.UnifiedDefinitions = json.RawMessage("[]")
	}
	if payload.UnifiedDefinitionHash == "" {
		payload.UnifiedDefinitionHash = store.EmptyUnifiedSetHash
	}
	if payload.UnifiedCodegenDescriptorHash == "" {
		payload.UnifiedCodegenDescriptorHash = store.EmptyUnifiedSetHash
	}
	if payload.UnifiedDefinitionSchemaVersion != unified.DefinitionSchemaVersion {
		return unifiedCompileError("unified_definition_version_unsupported")
	}
	if _, err := unified.DecodeDefinitions(payload.UnifiedDefinitions, unified.DefaultLimits()); err != nil {
		return unifiedCompileError("unified_definition_invalid")
	}
	definitionHash, err := unifiedCanonicalHash(payload.UnifiedDefinitions)
	if err != nil || definitionHash != payload.UnifiedDefinitionHash {
		return unifiedCompileError("unified_definition_hash_invalid")
	}
	return validateResolvedUnifiedDescriptor(payload)
}

// validateResolvedUnifiedDescriptor rejects malformed resolved unified descriptor before it can cross the SDK Unified compilation boundary.
func validateResolvedUnifiedDescriptor(payload *appResolvedPayload) error {
	if payload.UnifiedOperations == nil {
		if payload.UnifiedCodegenDescriptorHash != store.EmptyUnifiedSetHash {
			return unifiedCompileError("unified_descriptor_hash_invalid")
		}
		return nil
	}
	if payload.UnifiedOperations.SchemaVersion != models.SDKUnifiedDescriptorSchemaVersion {
		return unifiedCompileError("unified_descriptor_version_unsupported")
	}
	encoded, err := json.Marshal(payload.UnifiedOperations)
	if err != nil {
		return unifiedCompileError("unified_descriptor_invalid")
	}
	hash, err := unifiedCanonicalHash(encoded)
	if err != nil || hash != payload.UnifiedCodegenDescriptorHash {
		return unifiedCompileError("unified_descriptor_hash_invalid")
	}
	return nil
}

// unifiedCompileError wraps plan failures in the stable workspace error classification consumed by callers.
func unifiedCompileError(code string) error {
	return workspaceConfigHTTPError{status: http.StatusBadRequest, message: code, code: code, category: "validation"}
}

// recordSDKUnifiedCompile emits bounded compile counts and outcome labels without mapping or provider data.
func recordSDKUnifiedCompile(ctx context.Context, operationCount, bindingCount int, outcome string) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("engine.sdk_unified.compile", trace.WithAttributes(
		attribute.Int("unified.operation_count", operationCount),
		attribute.Int("unified.binding_count", bindingCount),
		attribute.String("outcome", outcome),
	))
}
