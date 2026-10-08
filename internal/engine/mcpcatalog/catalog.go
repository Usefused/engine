// Package mcpcatalog discovers definitions without executing provider tools or reading resource contents.
package mcpcatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
)

const MaxCatalogBytes = 8 << 20
const MaxItems = 2000

var ErrInvalidCatalog = errors.New("invalid or oversized MCP catalog")

type Catalog struct {
	ProtocolVersion   string            `json:"protocol_version"`
	Server            ServerInfo        `json:"server"`
	Supported         map[string]bool   `json:"supported"`
	Tools             []json.RawMessage `json:"tools"`
	Prompts           []json.RawMessage `json:"prompts"`
	Resources         []json.RawMessage `json:"resources"`
	ResourceTemplates []json.RawMessage `json:"resource_templates"`
}
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}
type Changes struct {
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
}

// Collections keeps protocol namespaces separate, including the two resource identity forms.
func (c Catalog) Collections() map[string][]json.RawMessage {
	return map[string][]json.RawMessage{"tools": c.Tools, "prompts": c.Prompts, "resources": c.Resources, "resource_templates": c.ResourceTemplates}
}

// itemKey uses exact protocol identity rather than a mutable display title.
func itemKey(kind string, raw json.RawMessage) (string, error) {
	var item struct {
		Name        string `json:"name"`
		URI         string `json:"uri"`
		URITemplate string `json:"uriTemplate"`
	}
	// Malformed definitions cannot produce a stable review diff.
	if json.Unmarshal(raw, &item) != nil || item.Name == "" {
		return "", ErrInvalidCatalog
	}
	key := item.Name
	// Resource identity is its URI rather than its non-unique human-readable name.
	switch kind {
	case "resources":
		key = item.URI
	case "resource_templates":
		key = item.URITemplate
	}
	// Bounded identities protect sorting, duplicate detection, and downstream UI keys.
	if key == "" || len(key) > 8192 {
		return "", ErrInvalidCatalog
	}
	return key, nil
}

// Normalize validates identities and sorts definitions so reordering pages never creates a false diff.
func (c *Catalog) Normalize() error {
	for kind, items := range c.Collections() {
		// A catalog has a bounded number of items independently in each namespace.
		if len(items) > MaxItems {
			return ErrInvalidCatalog
		}
		seen := map[string]bool{}
		for _, raw := range items {
			key, err := itemKey(kind, raw)
			// Duplicate upstream identities would make refresh and display ambiguous.
			if err != nil || seen[key] {
				return ErrInvalidCatalog
			}
			seen[key] = true
		}
		// Sort only the collection; JSON Schema array order remains untouched.
		sort.Slice(items, func(i, j int) bool { a, _ := itemKey(kind, items[i]); b, _ := itemKey(kind, items[j]); return a < b })
	}
	raw, err := json.Marshal(c)
	// The complete snapshot budget also bounds schemas, metadata, descriptions, and server identity.
	if err != nil || len(raw) > MaxCatalogBytes {
		return ErrInvalidCatalog
	}
	return nil
}

// Diff compares canonical JSON objects within each namespace while preserving meaningful array order.
func Diff(before, after Catalog) Changes {
	result := Changes{}
	for kind, items := range after.Collections() {
		previous := map[string][]byte{}
		for _, raw := range before.Collections()[kind] {
			key, _ := itemKey(kind, raw)
			previous[key] = canonicalItem(raw)
		}
		for _, raw := range items {
			key, _ := itemKey(kind, raw)
			old, exists := previous[key]
			// Definitions absent from the previous namespace count as additions.
			if !exists {
				result.Added++
			} else if !bytes.Equal(old, canonicalItem(raw)) {
				// Preserve array semantics while ignoring JSON object key ordering.
				result.Changed++
			}
			delete(previous, key)
		}
		result.Removed += len(previous)
	}
	return result
}

// canonicalItem ignores JSON object property order without losing large numeric schema values.
func canonicalItem(raw json.RawMessage) []byte {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	_ = decoder.Decode(&value)
	result, _ := json.Marshal(value)
	return result
}
