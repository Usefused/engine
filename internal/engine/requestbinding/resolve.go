// Package requestbinding compiles stored bucket binding sources into the
// concrete request values consumed by the dispatcher.
package requestbinding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type Resource struct {
	ProviderResourceID string
	BaseURL            string
	MetadataJSON       []byte
}

// HasDynamicSource lets dispatch avoid a resource lookup when every applicable
// binding was fully resolved during plan/apply.
func HasDynamicSource(bindings []store.WorkspaceConnectionBinding) bool {
	// Stop on the first dependency because callers only need to decide whether
	// selecting a connection resource is mandatory.
	for _, binding := range bindings {
		if binding.SourceKind == "connection_resource" {
			return true
		}
	}
	return false
}

// Resolve evaluates only the small structured source grammar persisted by the
// canonical validator; arbitrary templates never reach runtime dispatch.
// bindingBucketID is the dispatching bucket -- workspace connection bindings
// carry no bucket_id of their own (the profile that owns them is
// workspace-scoped, not bucket-scoped), but the returned store.BucketValue
// transport shape still carries a bucket identity for the credential merge
// path that consumes it, so the caller's already-resolved bucket supplies it.
func Resolve(bindings []store.WorkspaceConnectionBinding, resource Resource, bindingBucketID uuid.UUID) ([]store.BucketValue, error) {
	metadata, err := resourceMetadata(bindings, resource.MetadataJSON)
	if err != nil {
		return nil, err
	}
	values := make([]store.BucketValue, 0, len(bindings))
	// Resolve already-scoped rows independently so evaluation cannot alter SQL
	// target ordering or ownership.
	for _, binding := range bindings {
		value, err := resolveValue(binding, resource, metadata)
		if err != nil {
			return nil, err
		}
		values = append(values, store.BucketValue{
			ID: binding.ID, BucketID: bindingBucketID,
			ServiceID: binding.ServiceID, KeyName: binding.TargetName,
			Location: binding.TargetLocation, Value: value, Mode: binding.Mode,
			SourceKind: binding.SourceKind,
		})
	}
	return values, nil
}

// resourceMetadata avoids parsing provider metadata unless a configured source
// actually needs it, reducing both work and the amount of provider data held.
func resourceMetadata(bindings []store.WorkspaceConnectionBinding, raw []byte) (map[string]any, error) {
	// Provider metadata stays unmaterialized unless a binding explicitly consumes it.
	if !needsMetadata(bindings) {
		return nil, nil
	}
	metadata := map[string]any{}
	// Missing metadata is allowed here; required lookup produces the precise error.
	if len(raw) == 0 {
		return metadata, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&metadata); err != nil {
		return nil, errors.New("selected resource metadata is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("selected resource metadata is invalid")
	}
	return metadata, nil
}

// needsMetadata keeps the decision about provider metadata use in one place so
// future source kinds cannot accidentally make every request decode it.
func needsMetadata(bindings []store.WorkspaceConnectionBinding) bool {
	// The compiled source path, rather than the transport target, owns this decision.
	for _, binding := range bindings {
		if binding.SourcePath != nil && strings.HasPrefix(*binding.SourcePath, "metadata.") {
			return true
		}
	}
	return false
}

// resolveValue accepts only sources compiled by profile validation. Keeping the
// grammar closed prevents stored values from becoming a general template engine.
func resolveValue(binding store.WorkspaceConnectionBinding, resource Resource, metadata map[string]any) (string, error) {
	// Literals bypass resource selection and preserve their stored value exactly.
	if binding.SourceKind == "literal" && binding.LiteralValue != nil {
		return *binding.LiteralValue, nil
	}
	// Any other source shape indicates corrupted or pre-validation storage.
	if binding.SourceKind != "connection_resource" || binding.SourcePath == nil {
		return "", errors.New("bucket binding source is invalid")
	}
	// Built-in resource fields remain distinct from declared metadata keys.
	switch *binding.SourcePath {
	case "provider_resource_id":
		return requiredResourceValue(resource.ProviderResourceID)
	case "base_url":
		return requiredResourceValue(resource.BaseURL)
	default:
		return metadataValue(*binding.SourcePath, metadata)
	}
}

// requiredResourceValue fails closed when a profile expects routing context the
// selected provider resource does not contain.
func requiredResourceValue(value string) (string, error) {
	// Empty routing context fails closed instead of emitting an ambiguous request.
	if value == "" {
		return "", errors.New("selected resource does not contain a required binding value")
	}
	return value, nil
}

// metadataValue permits only scalar values because structured values have no
// unambiguous representation in headers, paths, or query parameters.
func metadataValue(path string, metadata map[string]any) (string, error) {
	key := strings.TrimPrefix(path, "metadata.")
	// Only compiled metadata paths may address the provider metadata object.
	if key == path || key == "" {
		return "", errors.New("selected resource binding path is invalid")
	}
	value, ok := metadata[key]
	// Required dynamic values cannot degrade to an empty transport value.
	if !ok || value == nil {
		return "", errors.New("selected resource does not contain a required metadata binding")
	}
	// Structured JSON has no stable header, path, query, or body representation.
	switch value.(type) {
	case string, json.Number, bool:
		return fmt.Sprint(value), nil
	default:
		return "", errors.New("selected resource metadata binding must be scalar")
	}
}
