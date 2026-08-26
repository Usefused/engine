package sandbox

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/Usefused/engine/internal/shared/models"
)

const (
	maxMCPSchemaEncodedBytes   = 512 << 10
	maxMCPSchemaDepth          = 32
	maxMCPSchemaNodes          = 10_000
	maxMCPAggregateSchemaNodes = 100_000
)

const (
	mcpSchemaInvalidCode             = "mcp_schema_invalid"
	mcpSchemaEncodedBytesLimitCode   = "mcp_schema_encoded_bytes_limit_exceeded"
	mcpSchemaDepthLimitCode          = "mcp_schema_depth_limit_exceeded"
	mcpSchemaNodeLimitCode           = "mcp_schema_node_limit_exceeded"
	mcpSchemaAggregateNodesLimitCode = "mcp_schema_aggregate_nodes_limit_exceeded"
)

// MCPSchemaAdmissionError exposes only a stable classification so rejected
// catalogue schemas cannot leak their authored content through errors or OTEL.
type MCPSchemaAdmissionError struct {
	Code string
}

// Error returns the stable machine classification without schema fragments.
func (e *MCPSchemaAdmissionError) Error() string {
	return e.Code
}

// Is lets callers classify wrapped admission failures without depending on
// error strings or attaching unsafe schema context.
func (e *MCPSchemaAdmissionError) Is(target error) bool {
	other, ok := target.(*MCPSchemaAdmissionError)
	// Codes, rather than pointer identity, keep separately wrapped instances stable.
	return ok && e.Code == other.Code
}

var (
	ErrMCPSchemaInvalid             = &MCPSchemaAdmissionError{Code: mcpSchemaInvalidCode}
	ErrMCPSchemaEncodedBytesLimit   = &MCPSchemaAdmissionError{Code: mcpSchemaEncodedBytesLimitCode}
	ErrMCPSchemaDepthLimit          = &MCPSchemaAdmissionError{Code: mcpSchemaDepthLimitCode}
	ErrMCPSchemaNodeLimit           = &MCPSchemaAdmissionError{Code: mcpSchemaNodeLimitCode}
	ErrMCPSchemaAggregateNodesLimit = &MCPSchemaAdmissionError{Code: mcpSchemaAggregateNodesLimitCode}
)

// mcpSchemaAdmission owns one fixture-wide aggregate budget while each schema
// retains independent byte, depth, and node ceilings.
type mcpSchemaAdmission struct {
	aggregateNodes int
	encodings      []models.RequestEncoding
}

// validateMCPFixtureSchemas rejects an unsafe catalogue before it can be
// indexed, written to the runtime, or exposed through search_docs.
func validateMCPFixtureSchemas(fixture *Fixture) error {
	// A missing fixture is invalid state rather than an empty authorized catalogue.
	if fixture == nil {
		return ErrMCPSchemaInvalid
	}
	admission := &mcpSchemaAdmission{}
	for index := range fixture.Operations {
		// Physical and logical schemas consume the same session-wide budget.
		if err := admission.admitOperation(&fixture.Operations[index]); err != nil {
			return err
		}
	}
	// An absent logical descriptor is the canonical empty logical schema set.
	if fixture.UnifiedOperations == nil {
		return nil
	}
	for index := range fixture.UnifiedOperations.Operations {
		// Unified descriptors are documentation schemas, not a separate trust boundary.
		if err := admission.admitUnifiedOperation(&fixture.UnifiedOperations.Operations[index]); err != nil {
			return err
		}
	}
	return nil
}

// admitOperation scans every schema-bearing physical contract, including
// nested media encodings that are easy to omit from a top-level check.
func (a *mcpSchemaAdmission) admitOperation(operation *FixtureOperation) error {
	for index := range operation.Parameters {
		// Each parameter can carry either a direct schema or media-type content schemas.
		if err := a.admitParameter(&operation.Parameters[index]); err != nil {
			return err
		}
	}
	// Operations without request content still retain response schema admission.
	if operation.RequestContent != nil {
		for index := range operation.RequestContent.Representations {
			// Request representations and their item contracts share one scanner.
			if err := a.admitRequestRepresentation(&operation.RequestContent.Representations[index]); err != nil {
				return err
			}
		}
	}
	// Map iteration order cannot affect admission because failures expose only stable codes.
	for status := range operation.Responses {
		response := operation.Responses[status]
		// Header schemas are part of the documented response contract.
		if err := a.admitHeaders(response.Headers); err != nil {
			return err
		}
		for index := range response.Representations {
			// Every response representation is independently selectable by documentation clients.
			if err := a.admitResponseRepresentation(&response.Representations[index]); err != nil {
				return err
			}
		}
	}
	return a.drainEncodings()
}

// admitUnifiedOperation applies the same raw-JSON policy to the public input,
// final output, and per-target binding result schemas.
func (a *mcpSchemaAdmission) admitUnifiedOperation(operation *models.SDKUnifiedOperationDescriptor) error {
	if err := a.admitRawSchema(operation.InputSchema); err != nil {
		return err
	}
	// Omitted output schemas intentionally represent all-settled methods.
	if len(operation.OutputSchema) > 0 {
		if err := a.admitRawSchema(operation.OutputSchema); err != nil {
			return err
		}
	}
	for index := range operation.Targets {
		// Target output contracts remain model-visible even when no final output is configured.
		if len(operation.Targets[index].OutputSchema) > 0 {
			if err := a.admitRawSchema(operation.Targets[index].OutputSchema); err != nil {
				return err
			}
		}
	}
	return nil
}

// admitParameter admits direct and content-based schemas without guessing
// which representation a later invocation will select.
func (a *mcpSchemaAdmission) admitParameter(parameter *models.Parameter) error {
	if err := a.admitSchemaContract(parameter.Schema); err != nil {
		return err
	}
	for mediaType := range parameter.Content {
		content := parameter.Content[mediaType]
		// Media-type keys are routing metadata and never enter admission errors.
		if err := a.admitParameterContent(&content); err != nil {
			return err
		}
	}
	return nil
}

// admitParameterContent covers container and item schemas before queuing
// recursive encoding metadata for iterative inspection.
func (a *mcpSchemaAdmission) admitParameterContent(content *models.ParameterContent) error {
	if err := a.admitSchemaPair(content.Schema, content.ItemSchema); err != nil {
		return err
	}
	return a.enqueueEncodings(content.Encoding, content.PrefixEncoding, content.ItemEncoding)
}

// admitRequestRepresentation admits request container/item contracts and
// defers nested header encodings to the shared iterative queue.
func (a *mcpSchemaAdmission) admitRequestRepresentation(representation *models.RequestRepresentation) error {
	if err := a.admitSchemaPair(representation.Schema, representation.ItemSchema); err != nil {
		return err
	}
	return a.enqueueEncodings(representation.Encoding, representation.PrefixEncoding, representation.ItemEncoding)
}

// admitResponseRepresentation admits response container/item contracts and
// queues its recursive prefix/item encoding metadata.
func (a *mcpSchemaAdmission) admitResponseRepresentation(representation *models.ResponseRepresentation) error {
	if err := a.admitSchemaPair(representation.Schema, representation.ItemSchema); err != nil {
		return err
	}
	return a.enqueueEncodings(nil, representation.PrefixEncoding, representation.ItemEncoding)
}

// admitSchemaPair keeps repeated container/item admission ordering consistent
// across parameter, request, and response representations.
func (a *mcpSchemaAdmission) admitSchemaPair(schema, itemSchema *models.SchemaContract) error {
	if err := a.admitSchemaContract(schema); err != nil {
		return err
	}
	return a.admitSchemaContract(itemSchema)
}

// admitHeaders scans direct and content-based header schemas without exposing
// header names in bounded admission failures.
func (a *mcpSchemaAdmission) admitHeaders(headers map[string]models.HeaderContract) error {
	for name := range headers {
		header := headers[name]
		// Direct header schemas and alternative content schemas are equally executable.
		if err := a.admitSchemaContract(header.Schema); err != nil {
			return err
		}
		for mediaType := range header.Content {
			content := header.Content[mediaType]
			// Nested content may enqueue additional header encodings for later iteration.
			if err := a.admitParameterContent(&content); err != nil {
				return err
			}
		}
	}
	return nil
}

// enqueueEncodings reserves all pending work before growing the explicit
// worklist, so wide or branching cyclic graphs cannot outgrow the visit budget.
func (a *mcpSchemaAdmission) enqueueEncodings(encodings map[string]models.RequestEncoding, prefix []models.RequestEncoding, item *models.RequestEncoding) error {
	// Reservation precedes every append, including a single oversized prefix slice.
	if err := a.reserveEncodingWork(len(encodings), len(prefix), item != nil); err != nil {
		return err
	}
	for name := range encodings {
		// Encoding names are irrelevant to schema complexity and stay out of errors.
		a.encodings = append(a.encodings, encodings[name])
	}
	a.encodings = append(a.encodings, prefix...)
	// A missing item encoding contributes no nested schema surface.
	if item != nil {
		a.encodings = append(a.encodings, *item)
	}
	return nil
}

// reserveEncodingWork keeps pending and completed work under one ceiling
// without adding untrusted lengths before checking for integer overflow.
func (a *mcpSchemaAdmission) reserveEncodingWork(mapped, prefixed int, hasItem bool) error {
	remaining := maxMCPAggregateSchemaNodes - a.aggregateNodes - len(a.encodings)
	// Map fanout is rejected before iteration or allocation of queued copies.
	if mapped > remaining {
		return ErrMCPSchemaAggregateNodesLimit
	}
	remaining -= mapped
	// Prefix slices consume the same reservation as map-based encodings.
	if prefixed > remaining {
		return ErrMCPSchemaAggregateNodesLimit
	}
	remaining -= prefixed
	// The optional item also needs a slot before its descendants can be visited.
	if hasItem && remaining < 1 {
		return ErrMCPSchemaAggregateNodesLimit
	}
	return nil
}

// drainEncodings iteratively scans nested encoding headers and descendants,
// clearing processed entries so the queue does not retain catalogue objects.
func (a *mcpSchemaAdmission) drainEncodings() error {
	// A worklist makes nested encoding depth independent from call-stack depth.
	for len(a.encodings) > 0 {
		// Programmatic fixtures can contain cyclic encoding maps even though JSON
		// fixtures cannot, so each visit consumes the finite aggregate work budget.
		if a.aggregateNodes >= maxMCPAggregateSchemaNodes {
			return ErrMCPSchemaAggregateNodesLimit
		}
		a.aggregateNodes++
		index := len(a.encodings) - 1
		encoding := a.encodings[index]
		a.encodings[index] = models.RequestEncoding{}
		a.encodings = a.encodings[:index]
		// Headers can recursively introduce content and further encodings.
		if err := a.admitHeaders(encoding.Headers); err != nil {
			return err
		}
		// Child reservations include siblings already pending in the worklist.
		if err := a.enqueueEncodings(encoding.Encoding, encoding.PrefixEncoding, encoding.ItemEncoding); err != nil {
			return err
		}
	}
	return nil
}

// admitSchemaContract bounds both the authoritative raw schema and its runtime
// projection because either representation can be consumed independently.
func (a *mcpSchemaAdmission) admitSchemaContract(contract *models.SchemaContract) error {
	// An omitted contract is not an implicit schema document.
	if contract == nil {
		return nil
	}
	return a.admitSchemaParts(contract.Raw, contract.Projection)
}

// admitSchemaParts shares raw/projection admission between immutable source
// contracts and their runtime model without converting either shape first.
func (a *mcpSchemaAdmission) admitSchemaParts(raw json.RawMessage, projection any) error {
	// Raw is optional for legacy fixtures, but must be admitted when present.
	if len(raw) > 0 {
		if err := a.admitRawSchema(raw); err != nil {
			return err
		}
	}
	nodes, err := measureMCPProjection(projection)
	// Projection work is admitted before any recursive whole-document encoding occurs.
	if err != nil {
		return err
	}
	return a.admitSchemaNodes(nodes)
}

// admitRawSchema charges one schema's structural work against its independent
// ceilings and the enclosing fixture's aggregate node budget.
func (a *mcpSchemaAdmission) admitRawSchema(raw json.RawMessage) error {
	nodes, err := measureMCPSchema(raw)
	// Per-schema failures remain authoritative and do not charge the shared budget.
	if err != nil {
		return err
	}
	return a.admitSchemaNodes(nodes)
}

// admitSchemaNodes charges a completed schema without consuming work already
// reserved for pending encoding branches.
func (a *mcpSchemaAdmission) admitSchemaNodes(nodes int) error {
	// Pending work remains part of the aggregate invariant until it is visited.
	if nodes > maxMCPAggregateSchemaNodes-a.aggregateNodes-len(a.encodings) {
		return ErrMCPSchemaAggregateNodesLimit
	}
	a.aggregateNodes += nodes
	return nil
}

// measureMCPSchema scans JSON tokens iteratively, treating member names as
// structural work so wide objects are bounded as well as deeply nested ones.
func measureMCPSchema(raw json.RawMessage) (int, error) {
	return measureMCPJSONSchema(raw, 0)
}

// measureMCPJSONSchema reuses one token scanner for standalone schemas and
// raw examples embedded inside an already-nested runtime projection.
func measureMCPJSONSchema(raw json.RawMessage, parentDepth int) (int, error) {
	// The byte ceiling runs before parsing to cap all later decoder work.
	if len(raw) > maxMCPSchemaEncodedBytes {
		return 0, ErrMCPSchemaEncodedBytesLimit
	}
	// Empty or syntactically invalid schemas fail before any runtime serialization.
	if len(raw) == 0 || !json.Valid(raw) {
		return 0, ErrMCPSchemaInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// JSON-number tokens remain lexical so valid large schema bounds do not overflow float64.
	decoder.UseNumber()
	depth, nodes := parentDepth, 0
	// Token iteration bounds nesting without recursively materializing the schema tree.
	for {
		token, err := decoder.Token()
		// EOF after one json.Valid document completes the iterative scan.
		if err == io.EOF {
			break
		}
		// Decoder failures are deliberately collapsed without raw input context.
		if err != nil {
			return 0, ErrMCPSchemaInvalid
		}
		delta, charge := mcpSchemaTokenCost(token)
		depth += delta
		// Negative depth would mean the validated document and decoder disagreed.
		if depth < 0 {
			return 0, ErrMCPSchemaInvalid
		}
		// Depth is checked at opening delimiters before child tokens are processed.
		if depth > maxMCPSchemaDepth {
			return 0, ErrMCPSchemaDepthLimit
		}
		nodes += charge
		// Charging keys as nodes prevents extremely wide property maps bypassing the limit.
		if nodes > maxMCPSchemaNodes {
			return 0, ErrMCPSchemaNodeLimit
		}
	}
	return nodes, nil
}

// mcpSchemaTokenCost maps decoder delimiters to iterative depth changes while
// charging every opening container, property name, and scalar as one node.
func mcpSchemaTokenCost(token json.Token) (int, int) {
	delimiter, ok := token.(json.Delim)
	// Scalar values and property names both represent bounded parsing work.
	if !ok {
		return 0, 1
	}
	// Opening and closing delimiters have opposite depth effects; only openings add nodes.
	switch delimiter {
	case '{', '[':
		return 1, 1
	case '}', ']':
		return -1, 0
	default:
		return 0, 0
	}
}
