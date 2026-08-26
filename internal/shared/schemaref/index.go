// Package schemaref indexes immutable schema definitions without expanding reference graphs.
package schemaref

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	MaxDefinitions = 2048
	MaxBytes       = 32 << 20
	MaxNodes       = 500_000
	MaxDepth       = 64
)

var ErrInvalid = errors.New("shared schema definitions are invalid or exceed limits")

// ErrLimit preserves generic invalidity while distinguishing admission budgets from broken references.
var ErrLimit = fmt.Errorf("%w: resource budget exceeded", ErrInvalid)

// Index owns one decoded dictionary per immutable service version; callers must not mutate its nodes.
type Index struct{ roots map[string]any }

// New checks every dictionary edge once; recursive schemas remain references, never expanded copies.
func New(definitions map[string]json.RawMessage) (*Index, error) {
	// Admission precedes allocation so dictionary width cannot bypass schema bounds.
	if len(definitions) > MaxDefinitions {
		return nil, ErrLimit
	}
	index := &Index{roots: make(map[string]any, len(definitions))}
	remaining := MaxBytes
	for name, raw := range definitions {
		// Names are exact JSON Pointer tokens, not identifiers normalized by the consumer.
		if name == "" || len(name) > 2048 {
			return nil, ErrInvalid
		}
		// Byte admission is a resource failure, not an unresolved schema identity.
		if len(raw) > remaining {
			return nil, ErrLimit
		}
		remaining -= len(raw)
		root, err := decode(raw)
		// Malformed definitions cannot become executable through an otherwise valid root.
		if err != nil {
			return nil, err
		}
		index.roots[name] = root
	}
	budget := MaxNodes
	for _, root := range index.roots {
		// Edges are checked against the completed dictionary, allowing forward and cyclic references.
		if err := index.validateNode(root, root, true, 0, &budget); err != nil {
			return nil, err
		}
	}
	return index, nil
}

// Validate checks a compact root without traversing already-admitted shared definitions again.
func (index *Index) Validate(raw json.RawMessage, shared bool) error {
	// Standalone schema behavior remains unchanged at this additive capability boundary.
	if !shared {
		return nil
	}
	// A marker without an attached dictionary is never interpreted as a free-form schema.
	if index == nil || len(index.roots) == 0 {
		return ErrInvalid
	}
	// A valid scope can still exceed its independent root byte budget.
	if len(raw) > MaxBytes {
		return ErrLimit
	}
	root, err := decode(raw)
	// Canonical envelope validation is separate, but this public helper still rejects malformed JSON.
	if err != nil {
		return err
	}
	budget := MaxNodes
	return index.validateNode(root, root, true, 0, &budget)
}

// Resolve gives local definitions precedence and returns a new document scope only for a shared lookup.
func (index *Index) Resolve(root any, ref string) (any, any, string, bool) {
	// A standalone document retains its authored pointer semantics even beside a shared dictionary.
	if node, ok := ResolveLocal(root, ref); ok {
		return node, root, "", true
	}
	// Only the negotiated dictionary namespace may escape the current raw schema root.
	if index == nil || !strings.HasPrefix(ref, "#/$defs/") {
		return nil, nil, "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(ref, "#/$defs/"), "/", 2)
	name, ok := pointerToken(parts[0])
	// Invalid escaping must not alias a differently authored definition name.
	if !ok {
		return nil, nil, "", false
	}
	document, ok := index.roots[name]
	// Missing references fail closed rather than degrading to the lossy projection.
	if !ok {
		return nil, nil, "", false
	}
	node := document
	// Subschema pointers stay relative to their owning definition's raw root.
	if len(parts) == 2 {
		node, ok = ResolveLocal(document, "#/"+parts[1])
	}
	return node, document, name, ok
}

// ResolveLocal supports exact local JSON Pointers without remote fetches or schema reconstruction.
func ResolveLocal(root any, ref string) (any, bool) {
	// Root references are legal and remain cycle-bounded by consuming traversals.
	if ref == "#" {
		return root, true
	}
	// External URIs and anchors need separate negotiated semantics, not implicit network I/O.
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	current := root
	for _, encoded := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token, valid := pointerToken(encoded)
		// Schema paths must identify authored object members, never inherited or coercible values.
		if !valid {
			return nil, false
		}
		var ok bool
		current, ok = pointerChild(current, token)
		// A missing member cannot silently resolve to null.
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// pointerChild supports schema subschema pointers through objects and composition arrays.
func pointerChild(value any, token string) (any, bool) {
	// JSON Pointer array indexes are canonical decimal positions, not numeric coercions.
	switch value := value.(type) {
	case map[string]any:
		child, ok := value[token]
		return child, ok
	case []any:
		position, err := strconv.Atoi(token)
		// Leading zeros, signs, and out-of-range positions cannot alias an authored index.
		if err != nil || position < 0 || position >= len(value) || strconv.Itoa(position) != token {
			return nil, false
		}
		return value[position], true
	default:
		return nil, false
	}
}

// pointerToken decodes RFC 6901 escapes and rejects malformed aliases.
func pointerToken(value string) (string, bool) {
	for position := 0; position < len(value); position++ {
		// Ordinary UTF-8 bytes are preserved exactly rather than normalized.
		if value[position] != '~' {
			continue
		}
		// A literal tilde must use ~0; accepting other escapes creates ambiguous identity.
		if position+1 >= len(value) || (value[position+1] != '0' && value[position+1] != '1') {
			return "", false
		}
		position++
	}
	return strings.ReplaceAll(strings.ReplaceAll(value, "~1", "/"), "~0", "~"), true
}

// decode retains exact numeric values because shared definitions are authoritative schema truth.
func decode(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root any
	// Shape validation runs separately so boolean schemas remain valid.
	if err := decoder.Decode(&root); err != nil {
		return nil, ErrInvalid
	}
	// The dictionary must contain exactly one JSON value, not a valid prefix plus ignored content.
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, ErrInvalid
	}
	return root, nil
}
