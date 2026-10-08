package mcpcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Discover negotiates the official SDK's supported protocols using a private, bounded HTTPS client.
func Discover(ctx context.Context, endpoint, token string) (Catalog, error) {
	// Reject unsafe endpoints before constructing a credential-bearing transport.
	if err := ValidateEndpoint(endpoint); err != nil {
		return Catalog{}, err
	}
	client, closeIdle := discoveryHTTPClient(endpoint, token)
	defer closeIdle()
	catalog, err := discoverWithClient(ctx, endpoint, client)
	// Reflected bearer material must not enter a persisted definition or browser response.
	if err != nil {
		return Catalog{}, err
	}
	raw, _ := json.Marshal(catalog)
	encoded, _ := json.Marshal(token)
	// Reflected credential material is rejected before catalog data leaves the discovery boundary.
	if token != "" && (bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, encoded[1:len(encoded)-1])) {
		return Catalog{}, ErrDiscovery
	}
	return catalog, nil
}

// discoverWithClient isolates protocol discovery so tests can use a local fixture without weakening production egress policy.
func discoverWithClient(ctx context.Context, endpoint string, httpClient *http.Client) (Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "Fused", Version: "1.0.0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: 2 << 20}, nil)
	// A failed negotiation produces no partial catalog and no provider-auth error text.
	if err != nil {
		return Catalog{}, ErrDiscovery
	}
	defer session.Close()
	initialized := session.InitializeResult()
	// Both legacy initialization and modern discovery must supply server identity and capabilities.
	if initialized == nil || initialized.Capabilities == nil || initialized.ServerInfo == nil {
		return Catalog{}, ErrDiscovery
	}
	info, caps := initialized.ServerInfo, initialized.Capabilities
	catalog := Catalog{ProtocolVersion: initialized.ProtocolVersion, Server: ServerInfo{Name: info.Name, Version: info.Version, Title: info.Title}, Supported: map[string]bool{"tools": caps.Tools != nil, "prompts": caps.Prompts != nil, "resources": caps.Resources != nil, "resource_templates": caps.Resources != nil}, Tools: []json.RawMessage{}, Prompts: []json.RawMessage{}, Resources: []json.RawMessage{}, ResourceTemplates: []json.RawMessage{}}
	// All four lists complete before the snapshot is eligible for validation and persistence.
	if err = discoverCollections(ctx, session, &catalog); err != nil {
		return Catalog{}, err
	}
	// Invalid identities and oversized schemas must never reach a reviewable snapshot.
	if err = catalog.Normalize(); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

// discoverCollections shares one byte budget while preserving capability absence as an empty unsupported category.
func discoverCollections(ctx context.Context, session *mcp.ClientSession, catalog *Catalog) error {
	budget := 0
	var err error
	catalog.Tools, err = collectOptional(ctx, &budget, catalog.Supported["tools"], toolPage(session))
	// Every supported list is mandatory for a complete reviewable snapshot.
	if err != nil {
		return ErrDiscovery
	}
	catalog.Prompts, err = collectOptional(ctx, &budget, catalog.Supported["prompts"], promptPage(session))
	// Prompts cannot be silently omitted when the provider advertises them.
	if err != nil {
		return ErrDiscovery
	}
	catalog.Resources, err = collectOptional(ctx, &budget, catalog.Supported["resources"], resourcePage(session))
	// Fixed resources and templates contribute independently to the catalog.
	if err != nil {
		return ErrDiscovery
	}
	catalog.ResourceTemplates, err = collectOptional(ctx, &budget, catalog.Supported["resource_templates"], templatePage(session))
	// A template discovery failure invalidates the complete preview just like the other lists.
	if err != nil {
		return ErrDiscovery
	}
	return nil
}

// collectOptional avoids sending unsupported protocol methods and always returns a JSON array for the UI.
func collectOptional[T any](ctx context.Context, budget *int, supported bool, list listPage[T]) ([]json.RawMessage, error) {
	// Capability absence is a valid state, unlike a failed request for an advertised capability.
	if !supported {
		return []json.RawMessage{}, nil
	}
	return collectPages(ctx, budget, list)
}

type listPage[T any] func(context.Context, string) ([]T, string, error)

// collectPages bounds pagination, repeated cursors, definitions, and total serialized bytes across all four lists.
func collectPages[T any](ctx context.Context, budget *int, list listPage[T]) ([]json.RawMessage, error) {
	items := []json.RawMessage{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 50; page++ {
		values, next, err := list(ctx, cursor)
		// A page failure invalidates the complete preview instead of approving a partial capability set.
		if err != nil || len(items)+len(values) > MaxItems || len(next) > 8192 {
			return nil, ErrDiscovery
		}
		for _, value := range values {
			raw, err := json.Marshal(value)
			*budget += len(raw)
			// The shared budget prevents several individually small pages exhausting Engine memory.
			if err != nil || *budget > MaxCatalogBytes {
				return nil, ErrInvalidCatalog
			}
			items = append(items, raw)
		}
		// An absent cursor is the only successful end-of-list marker.
		if next == "" {
			return items, nil
		}
		// Repeated cursors otherwise keep discovery alive until its deadline with duplicate results.
		if seen[next] {
			return nil, ErrDiscovery
		}
		seen[next] = true
		cursor = next
	}
	return nil, ErrDiscovery
}

// toolPage adapts the official SDK result without projecting JSON Schema keywords into REST parameters.
func toolPage(s *mcp.ClientSession) listPage[*mcp.Tool] {
	return func(ctx context.Context, cursor string) ([]*mcp.Tool, string, error) {
		result, err := s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		// Do not dereference a missing result on protocol failure.
		if err != nil {
			return nil, "", err
		}
		return result.Tools, result.NextCursor, nil
	}
}

// promptPage retains prompt argument metadata without evaluating a prompt.
func promptPage(s *mcp.ClientSession) listPage[*mcp.Prompt] {
	return func(ctx context.Context, cursor string) ([]*mcp.Prompt, string, error) {
		result, err := s.ListPrompts(ctx, &mcp.ListPromptsParams{Cursor: cursor})
		// Do not dereference a missing result on protocol failure.
		if err != nil {
			return nil, "", err
		}
		return result.Prompts, result.NextCursor, nil
	}
}

// resourcePage lists resource metadata without following or reading resource URIs.
func resourcePage(s *mcp.ClientSession) listPage[*mcp.Resource] {
	return func(ctx context.Context, cursor string) ([]*mcp.Resource, string, error) {
		result, err := s.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cursor})
		// Do not dereference a missing result on protocol failure.
		if err != nil {
			return nil, "", err
		}
		return result.Resources, result.NextCursor, nil
	}
}

// templatePage retains parameterized resource definitions as a separate namespace.
func templatePage(s *mcp.ClientSession) listPage[*mcp.ResourceTemplate] {
	return func(ctx context.Context, cursor string) ([]*mcp.ResourceTemplate, string, error) {
		result, err := s.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{Cursor: cursor})
		// Do not dereference a missing result on protocol failure.
		if err != nil {
			return nil, "", err
		}
		return result.ResourceTemplates, result.NextCursor, nil
	}
}
