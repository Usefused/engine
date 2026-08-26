package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/google/uuid"
)

const (
	maxUnifiedOperations           = 64
	maxUnifiedBindingsPerOperation = 16
	maxUnifiedNameLength           = 256
	maxUnifiedDescriptionLength    = 4096
	maxUnifiedOperationIDLength    = 512
	maxUnifiedTargetLength         = 253
	maxUnifiedConfigBytes          = 1 << 20
)

var unifiedOperationNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)

type sdkUnifiedOperationDoc struct {
	Description string                          `json:"description,omitempty"`
	Input       json.RawMessage                 `json:"input"`
	Bindings    map[string]sdkUnifiedBindingDoc `json:"bindings"`
	Output      json.RawMessage                 `json:"output,omitempty"`
}

type sdkUnifiedBindingDoc struct {
	Operation string                 `json:"operation"`
	Service   string                 `json:"service,omitempty"`
	DependsOn []string               `json:"depends_on,omitempty"`
	Input     json.RawMessage        `json:"input,omitempty"`
	Output    json.RawMessage        `json:"output,omitempty"`
	Rollback  *sdkUnifiedRollbackDoc `json:"rollback,omitempty"`
}

// sdkUnifiedRollbackDoc is the public compensation declaration; exact service
// identity is inherited from its containing binding during compilation.
type sdkUnifiedRollbackDoc struct {
	Operation string          `json:"operation"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// UnmarshalJSON restores json only after strict shape, limit, and namespace checks.
func (b *sdkUnifiedBindingDoc) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 {
		return errors.New("Unified binding is empty")
	}
	if raw[0] == '"' {
		return json.Unmarshal(raw, &b.Operation)
	}
	type bindingAlias sdkUnifiedBindingDoc
	var decoded bindingAlias
	if err := decodeOneStrictJSON(bytes.NewReader(raw), &decoded); err != nil {
		return err
	}
	*b = sdkUnifiedBindingDoc(decoded)
	return nil
}

// validateSDKUnifiedOperations preserves SDK package-language checks on top of the shared Unified authoring contract.
func validateSDKUnifiedOperations(doc sdkConfigDocument) error {
	return validateAppUnifiedOperations(doc, true)
}

// validateAppUnifiedOperations keeps one graph validator for SDK and MCP while gating only package-codegen constraints.
func validateAppUnifiedOperations(doc sdkConfigDocument, requireCodegenNames bool) error {
	// Absence is the canonical empty graph and needs no operation-level checks.
	if len(doc.UnifiedOperations) == 0 {
		return nil
	}
	// The shared bound protects both generated packages and Engine-hosted MCP state.
	if len(doc.UnifiedOperations) > maxUnifiedOperations {
		return fmt.Errorf("sdk config supports at most %d Unified Operations", maxUnifiedOperations)
	}
	names := sortedUnifiedOperationNames(doc.UnifiedOperations)
	// Package-only restrictions remain layered over the common graph contract.
	if err := validateUnifiedCodegenContract(names, doc.Language, requireCodegenNames); err != nil {
		return err
	}
	for _, name := range names {
		// Each operation must be independently valid before whole-set hashing.
		if err := validateSDKUnifiedOperation(name, doc.UnifiedOperations[name], doc.Services); err != nil {
			return err
		}
	}
	return validateUnifiedEncodedSize(doc.UnifiedOperations)
}

// validateUnifiedCodegenContract isolates package-only language and generated-symbol admission from the shared graph rules.
func validateUnifiedCodegenContract(names []string, language string, required bool) error {
	// Exact MCP logical names never become language symbols, so package-only
	// restrictions must not narrow an otherwise valid Engine graph.
	if !required {
		return nil
	}
	// Generated Unified methods exist only in the two generators that implement
	// the public descriptor contract.
	if language != "typescript" && language != "python" {
		return errors.New("Unified Operations require TypeScript or Python SDK generation")
	}
	// Prefix collisions must fail before the finer normalized-symbol checks.
	if err := validateUnifiedOperationNameCollisions(names); err != nil {
		return err
	}
	return validateUnifiedGeneratedNames(names, language)
}

// validateUnifiedGeneratedNames rejects malformed unified generated names before it can cross the SDK Unified configuration boundary.
func validateUnifiedGeneratedNames(names []string, language string) error {
	if err := validateUnifiedNormalizedNames(names); err != nil {
		return err
	}
	if language == "python" {
		return validateUnifiedPythonSegments(names)
	}
	return nil
}

// validateUnifiedNormalizedNames rejects malformed unified normalized names before it can cross the SDK Unified configuration boundary.
func validateUnifiedNormalizedNames(names []string) error {
	types := make(map[string]string, len(names))
	namespaces := make(map[string]string)
	for _, name := range names {
		generated := unifiedGeneratedName(name)
		if previous, exists := types[generated]; exists && previous != name {
			return fmt.Errorf("Unified Operations %q and %q collide as generated type names", previous, name)
		}
		types[generated] = name
		if err := validateUnifiedNamespaceNames(name, namespaces); err != nil {
			return err
		}
	}
	return nil
}

// validateUnifiedNamespaceNames rejects malformed unified namespace names before it can cross the SDK Unified configuration boundary.
func validateUnifiedNamespaceNames(name string, seen map[string]string) error {
	segments := strings.Split(name, ".")
	for end := 1; end < len(segments); end++ {
		path := strings.Join(segments[:end], ".")
		generated := unifiedGeneratedName(path)
		if previous, exists := seen[generated]; exists && previous != path {
			return fmt.Errorf("Unified namespaces %q and %q collide after code generation", previous, path)
		}
		seen[generated] = path
	}
	return nil
}

// validateUnifiedPythonSegments rejects malformed unified python segments before it can cross the SDK Unified configuration boundary.
func validateUnifiedPythonSegments(names []string) error {
	for _, name := range names {
		for _, segment := range strings.Split(name, ".") {
			if isPythonKeyword(segment) {
				return fmt.Errorf("Unified Operation %q contains Python keyword segment %q", name, segment)
			}
		}
	}
	return nil
}

// unifiedGeneratedName normalizes public target segments to the identifier form emitted by generated SDKs.
func unifiedGeneratedName(value string) string {
	segments := strings.FieldsFunc(value, func(char rune) bool { return char == '.' || char == '_' })
	for index, segment := range segments {
		segments[index] = strings.ToUpper(segment[:1]) + segment[1:]
	}
	return strings.Join(segments, "")
}

// isPythonKeyword performs a bounded membership check used by SDK Unified configuration admission.
func isPythonKeyword(value string) bool {
	switch value {
	case "False", "None", "True", "and", "as", "assert", "async", "await", "break", "class", "continue", "def", "del", "elif", "else", "except", "finally", "for", "from", "global", "if", "import", "in", "is", "lambda", "nonlocal", "not", "or", "pass", "raise", "return", "try", "while", "with", "yield":
		return true
	default:
		return false
	}
}

// validateUnifiedOperationNameCollisions rejects malformed unified operation name collisions before it can cross the SDK Unified configuration boundary.
func validateUnifiedOperationNameCollisions(names []string) error {
	for index := 1; index < len(names); index++ {
		if strings.HasPrefix(names[index], names[index-1]+".") {
			return fmt.Errorf("Unified Operations %q and %q collide as generated namespace paths", names[index-1], names[index])
		}
	}
	return nil
}

// sortedUnifiedOperationNames provides deterministic declaration order for compilation, hashing, and result alignment.
func sortedUnifiedOperationNames(operations map[string]sdkUnifiedOperationDoc) []string {
	names := make([]string, 0, len(operations))
	for name := range operations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateSDKUnifiedOperation rejects malformed sdk unified operation before it can cross the SDK Unified configuration boundary.
func validateSDKUnifiedOperation(name string, operation sdkUnifiedOperationDoc, services map[string]sdkConfigServiceDoc) error {
	if len(name) > maxUnifiedNameLength || !unifiedOperationNamePattern.MatchString(name) {
		return fmt.Errorf("Unified Operation %q must use dot-separated identifier segments", name)
	}
	if len(operation.Description) > maxUnifiedDescriptionLength {
		return fmt.Errorf("Unified Operation %q description is too long", name)
	}
	if err := validateUnifiedSchema(name+" input", operation.Input); err != nil {
		return err
	}
	if len(operation.Bindings) == 0 || len(operation.Bindings) > maxUnifiedBindingsPerOperation {
		return fmt.Errorf("Unified Operation %q requires between 1 and %d bindings", name, maxUnifiedBindingsPerOperation)
	}
	for _, target := range sortedUnifiedBindingTargets(operation.Bindings) {
		binding := operation.Bindings[target]
		if err := validateSDKUnifiedBinding(name, target, binding, services); err != nil {
			return err
		}
	}
	if err := validateSDKUnifiedGraph(name, operation.Bindings); err != nil {
		return err
	}
	return validateUnifiedOutputMode(name, operation)
}

// validateSDKUnifiedBinding rejects malformed sdk unified binding before it can cross the SDK Unified configuration boundary.
func validateSDKUnifiedBinding(operationName, target string, binding sdkUnifiedBindingDoc, services map[string]sdkConfigServiceDoc) error {
	service, err := selectedUnifiedBindingService(operationName, target, binding.Service, services)
	if err != nil {
		return err
	}
	operationID := strings.TrimSpace(binding.Operation)
	if operationID == "" || len(operationID) > maxUnifiedOperationIDLength || strings.ContainsAny(operationID, "\r\n\x00") {
		return fmt.Errorf("Unified Operation %q binding %q requires operation", operationName, target)
	}
	if !service.SelectAll && !containsExact(service.Operations, operationID) {
		return fmt.Errorf("Unified Operation %q binding %q operation %q is not selected", operationName, target, operationID)
	}
	if err := validateUnifiedBindingInput(operationName, target, binding.Input); err != nil {
		return err
	}
	if err := validateSDKUnifiedRollback(operationName, target, binding.Rollback, service); err != nil {
		return err
	}
	return validateUnifiedOutput(operationName+" binding "+target, binding.Output)
}

// validateSDKUnifiedRollback keeps compensation on the same selected service
// and validates its private mapping without exposing it to Registry.
func validateSDKUnifiedRollback(operationName, target string, rollback *sdkUnifiedRollbackDoc, service sdkConfigServiceDoc) error {
	if rollback == nil {
		return nil
	}
	operationID := strings.TrimSpace(rollback.Operation)
	if operationID == "" || len(operationID) > maxUnifiedOperationIDLength || strings.ContainsAny(operationID, "\r\n\x00") {
		return fmt.Errorf("Unified Operation %q binding %q rollback requires operation", operationName, target)
	}
	// A rollback cannot widen the SDK selection even though it runs only on
	// a failure path.
	if !service.SelectAll && !containsExact(service.Operations, operationID) {
		return fmt.Errorf("Unified Operation %q binding %q rollback operation %q is not selected", operationName, target, operationID)
	}
	return validateUnifiedBindingInput(operationName+" rollback", target, rollback.Input)
}

// validateSDKUnifiedGraph rejects missing, duplicate, self, and cyclic edges
// before endpoint lookup or immutable app persistence.
func validateSDKUnifiedGraph(operationName string, bindings map[string]sdkUnifiedBindingDoc) error {
	targets := sortedUnifiedBindingTargets(bindings)
	definitions := make([]unifiedGraphBinding, 0, len(targets))
	for _, target := range targets {
		definitions = append(definitions, unifiedGraphBinding{target: target, dependencies: bindings[target].DependsOn})
	}
	if err := validateUnifiedDocGraph(definitions); err != nil {
		return fmt.Errorf("Unified Operation %q dependency graph is invalid: %w", operationName, err)
	}
	return nil
}

// selectedUnifiedBindingService resolves a graph step to its declared service
// while applying the binding-key default defined by the configuration contract.
func selectedUnifiedBindingService(operationName, target, configuredService string, services map[string]sdkConfigServiceDoc) (sdkConfigServiceDoc, error) {
	// Public execution identity must be representable safely in canonical audit metadata.
	if !unified.ValidPublicName(target, maxUnifiedTargetLength) {
		return sdkConfigServiceDoc{}, fmt.Errorf("Unified Operation %q binding target must be an exact configured service key", operationName)
	}
	serviceTarget := unifiedBindingServiceTarget(target, configuredService)
	// Exact service keys keep aliases from introducing normalized or ambiguous selector namespaces.
	if !unified.ValidPublicName(serviceTarget, maxUnifiedTargetLength) {
		return sdkConfigServiceDoc{}, fmt.Errorf("Unified Operation %q binding %q service must be an exact configured service key", operationName, target)
	}
	service, ok := services[serviceTarget]
	// An alias can reuse only a service already selected by the SDK declaration.
	if !ok {
		return sdkConfigServiceDoc{}, fmt.Errorf("Unified Operation %q binding %q service %q must match a configured service", operationName, target, serviceTarget)
	}
	return service, nil
}

// unifiedBindingServiceTarget uses the binding key when the optional service
// target is omitted, matching compact and expanded binding semantics.
func unifiedBindingServiceTarget(target, configuredService string) string {
	// Omission deliberately makes compact and expanded bindings select their graph key.
	if configuredService == "" {
		return target
	}
	return configuredService
}

// validateUnifiedBindingInput rejects malformed unified binding input before it can cross the SDK Unified configuration boundary.
func validateUnifiedBindingInput(operationName, target string, input json.RawMessage) error {
	if len(input) == 0 {
		return nil
	}
	if _, err := canonicaljson.Canonicalize(input); err != nil {
		return fmt.Errorf("Unified Operation %q binding %q input is invalid: %w", operationName, target, err)
	}
	return nil
}

// validateUnifiedOutputMode rejects malformed unified output mode before it can cross the SDK Unified configuration boundary.
func validateUnifiedOutputMode(name string, operation sdkUnifiedOperationDoc) error {
	if err := validateUnifiedOutput(name, operation.Output); err != nil {
		return err
	}
	for _, target := range sortedUnifiedBindingTargets(operation.Bindings) {
		binding := operation.Bindings[target]
		if err := validateUnifiedOutput(name+" binding "+target, binding.Output); err != nil {
			return err
		}
	}
	return nil
}

// validateUnifiedOutput rejects malformed recursive output before endpoint resolution begins.
func validateUnifiedOutput(name string, output json.RawMessage) error {
	if len(output) == 0 {
		return nil
	}
	if _, _, err := compileSDKUnifiedOutputDocument(output, nil); err != nil {
		return fmt.Errorf("%s output is invalid: %w", name, err)
	}
	return nil
}

// validateUnifiedSchema rejects malformed unified schema before it can cross the SDK Unified configuration boundary.
func validateUnifiedSchema(name string, schema json.RawMessage) error {
	if len(schema) == 0 {
		return fmt.Errorf("%s schema is required", name)
	}
	canonical, err := canonicaljson.Canonicalize(schema)
	if err != nil {
		return fmt.Errorf("%s schema is invalid: %w", name, err)
	}
	var shape map[string]any
	if json.Unmarshal(canonical, &shape) != nil || strings.TrimSpace(fmt.Sprint(shape["type"])) == "" {
		return fmt.Errorf("%s schema requires an object with type", name)
	}
	return nil
}

// validateUnifiedEncodedSize rejects malformed unified encoded size before it can cross the SDK Unified configuration boundary.
func validateUnifiedEncodedSize(operations map[string]sdkUnifiedOperationDoc) error {
	encoded, err := json.Marshal(operations)
	if err != nil {
		return errors.New("Unified Operations could not be encoded")
	}
	if len(encoded) > maxUnifiedConfigBytes {
		return fmt.Errorf("Unified Operations exceed maximum encoded size %d", maxUnifiedConfigBytes)
	}
	return nil
}

// containsExact performs a bounded membership check used by SDK Unified configuration admission.
func containsExact(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// unifiedBindingCount counts physical binding declarations for bounded plan telemetry.
func unifiedBindingCount(operations map[string]sdkUnifiedOperationDoc) int {
	count := 0
	for _, operation := range operations {
		count += len(operation.Bindings)
	}
	return count
}

// unifiedPhysicalOperationCount includes every forward and optional rollback
// endpoint so plan resolution can remain one set-based query.
func unifiedPhysicalOperationCount(operations map[string]sdkUnifiedOperationDoc) int {
	count := unifiedBindingCount(operations)
	for _, operation := range operations {
		for _, binding := range operation.Bindings {
			if binding.Rollback != nil {
				count++
			}
		}
	}
	return count
}

// validateUniqueUnifiedTargets rejects malformed unique unified targets before it can cross the SDK Unified configuration boundary.
func validateUniqueUnifiedTargets(services []sdkResolvedService) error {
	seen := make(map[[2]uuid.UUID]string, len(services))
	for _, service := range services {
		key := [2]uuid.UUID{service.ServiceID, service.ServiceVersionID}
		if previous, ok := seen[key]; ok && previous != service.PublicTarget {
			return fmt.Errorf("configured targets %q and %q resolve to the same service version", previous, service.PublicTarget)
		}
		seen[key] = service.PublicTarget
	}
	return nil
}

// canonicalizeUnifiedOperations normalizes SDK Unified configuration ordering and bytes so hashes are reproducible.
func canonicalizeUnifiedOperations(operations map[string]sdkUnifiedOperationDoc) map[string]sdkUnifiedOperationDoc {
	if len(operations) == 0 {
		return nil
	}
	canonical := make(map[string]sdkUnifiedOperationDoc, len(operations))
	for name, operation := range operations {
		operation.Description = strings.TrimSpace(operation.Description)
		operation.Input = canonicalUnifiedJSON(operation.Input)
		operation.Output = canonicalUnifiedJSON(operation.Output)
		bindings := make(map[string]sdkUnifiedBindingDoc, len(operation.Bindings))
		for target, binding := range operation.Bindings {
			binding.Operation = strings.TrimSpace(binding.Operation)
			binding.Service = strings.TrimSpace(binding.Service)
			binding.DependsOn = canonicalUnifiedDependencies(binding.DependsOn)
			binding.Input = canonicalUnifiedJSON(binding.Input)
			binding.Output = canonicalUnifiedJSON(binding.Output)
			binding.Rollback = canonicalUnifiedRollback(binding.Rollback)
			bindings[target] = binding
		}
		operation.Bindings = bindings
		canonical[name] = operation
	}
	return canonical
}

// canonicalUnifiedDependencies sorts dependency edges because declaration
// order has no scheduling semantics.
func canonicalUnifiedDependencies(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	canonical := append([]string(nil), values...)
	sort.Strings(canonical)
	return canonical
}

// canonicalUnifiedRollback normalizes the operation and mapping used in the
// immutable SDK source hash.
func canonicalUnifiedRollback(rollback *sdkUnifiedRollbackDoc) *sdkUnifiedRollbackDoc {
	if rollback == nil {
		return nil
	}
	copy := *rollback
	copy.Operation = strings.TrimSpace(copy.Operation)
	copy.Input = canonicalUnifiedJSON(copy.Input)
	return &copy
}

// canonicalUnifiedJSON normalizes SDK Unified configuration ordering and bytes so hashes are reproducible.
func canonicalUnifiedJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	canonical, err := canonicaljson.Canonicalize(raw)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return canonical
}
