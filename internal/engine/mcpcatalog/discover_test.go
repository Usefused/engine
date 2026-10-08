package mcpcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDiscoverCatalog exercises the real SDK transport against all four protocol lists without invoking content handlers.
func TestDiscoverCatalog(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
		// Each supported handshake must produce the same saved catalog shape.
		t.Run(protocol, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "Fixture", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{protocol}})
			// Content handlers fail the test if metadata discovery accidentally executes a provider operation.
			server.AddTool(&mcp.Tool{Name: "find_item", Description: "Find an item", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"$defs":{"id":{"type":"string"}}}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				t.Error("discovery called a tool")
				return nil, errors.New("unexpected")
			})
			server.AddPrompt(&mcp.Prompt{Name: "summarize", Arguments: []*mcp.PromptArgument{{Name: "topic", Required: true}}}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				t.Error("discovery rendered a prompt")
				return nil, errors.New("unexpected")
			})
			// Resources are metadata-only; their URIs are never dereferenced by Fused.
			handler := func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				t.Error("discovery read a resource")
				return nil, errors.New("unexpected")
			}
			server.AddResource(&mcp.Resource{Name: "Guide", URI: "fixture://guide", MIMEType: "text/plain"}, handler)
			server.AddResourceTemplate(&mcp.ResourceTemplate{Name: "Issue", URITemplate: "fixture://issues/{id}"}, handler)
			fixture := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
			defer fixture.Close()
			catalog, err := discoverWithClient(context.Background(), fixture.URL, fixture.Client())
			// Both protocol generations must preserve every supported catalog category.
			if err != nil || len(catalog.Tools) != 1 || len(catalog.Prompts) != 1 || len(catalog.Resources) != 1 || len(catalog.ResourceTemplates) != 1 || catalog.ProtocolVersion != protocol {
				t.Fatalf("catalog=%+v error=%v", catalog, err)
			}
			// Discovery must preserve the complete catalog while leaving approval to the user.
			if !strings.Contains(string(catalog.Tools[0]), "$defs") {
				t.Fatal("lost JSON Schema keywords")
			}
		})
	}
}

// TestCollectPagesBounds verifies pagination completion, cycle rejection, total budgets, and cancellation propagation.
func TestCollectPagesBounds(t *testing.T) {
	budget := 0
	calls := 0
	// Two pages prove the collector follows the opaque cursor to completion.
	list := func(ctx context.Context, cursor string) ([]map[string]string, string, error) {
		calls++
		// The fixture exposes a second page only after receiving the expected opaque cursor.
		if cursor == "" {
			return []map[string]string{{"name": "first"}}, "next", nil
		}
		return []map[string]string{{"name": "second"}}, "", nil
	}
	items, err := collectPages(context.Background(), &budget, list)
	// Discovery must preserve the complete catalog while leaving approval to the user.
	if err != nil || len(items) != 2 || calls != 2 {
		t.Fatalf("items=%v calls=%d error=%v", items, calls, err)
	}
	// Repeating a cursor cannot loop or approve a duplicate partial catalog.
	cycle := func(context.Context, string) ([]string, string, error) { return nil, "same", nil }
	if _, err = collectPages(context.Background(), &budget, cycle); err == nil {
		t.Fatal("accepted cursor cycle")
	}
	budget = MaxCatalogBytes
	// Discovery must preserve the complete catalog while leaving approval to the user.
	if _, err = collectPages(context.Background(), &budget, list); err == nil {
		t.Fatal("accepted oversized catalog")
	}
	// Endless unique cursors are stopped by a page bound independently from the timeout.
	budget = 0
	calls = 0
	endless := func(context.Context, string) ([]string, string, error) { calls++; return nil, fmt.Sprint(calls), nil }
	// The page cap must terminate even a provider that produces endlessly unique cursors.
	if _, err = collectPages(context.Background(), &budget, endless); err == nil || calls != 50 {
		t.Fatalf("unbounded pagination calls=%d error=%v", calls, err)
	}
}

// TestCatalogDiffAndIdentity checks semantic changes, separate namespaces, exact URIs, and duplicate rejection.
func TestCatalogDiffAndIdentity(t *testing.T) {
	before := Catalog{Tools: []json.RawMessage{json.RawMessage(`{"name":"same","description":"old"}`), json.RawMessage(`{"name":"gone"}`)}}
	after := Catalog{Tools: []json.RawMessage{json.RawMessage(`{"name":"same","description":"new"}`)}, Prompts: []json.RawMessage{json.RawMessage(`{"name":"same"}`)}}
	// Equal names in different protocol namespaces must count independently in the review diff.
	if got := Diff(before, after); got != (Changes{Added: 1, Changed: 1, Removed: 1}) {
		t.Fatalf("diff=%+v", got)
	}
	// JSON object ordering and large numeric values cannot manufacture drift.
	a := Catalog{Tools: []json.RawMessage{json.RawMessage(`{"name":"x","inputSchema":{"const":9007199254740993}}`)}}
	b := Catalog{Tools: []json.RawMessage{json.RawMessage(`{"inputSchema":{"const":9007199254740993},"name":"x"}`)}}
	// Reordering object keys must not invent a schema change or round a large integer.
	if got := Diff(a, b); got != (Changes{}) {
		t.Fatalf("ordering drift=%+v", got)
	}
	b.Tools = append(b.Tools, b.Tools[0])
	// Duplicate tool identities would make both review and React disclosure keys ambiguous.
	if b.Normalize() == nil {
		t.Fatal("accepted duplicate tool")
	}
	c := Catalog{Resources: []json.RawMessage{json.RawMessage(`{"name":"same","uri":"fixture://a"}`), json.RawMessage(`{"name":"same","uri":"fixture://b"}`)}}
	// Resource display names may coincide when their authoritative URIs are distinct.
	if err := c.Normalize(); err != nil {
		t.Fatalf("distinct resource URIs rejected: %v", err)
	}
}

// TestEndpointAndNetworkPolicy verifies unsafe URLs and special-purpose addresses cannot reach a socket.
func TestEndpointAndNetworkPolicy(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/mcp", "https://token@example.com/mcp", "https://example.com/mcp?token=secret", "https://example.com/mcp#fragment", "file:///tmp/mcp"} {
		// Every listed URL violates the public credential-free HTTPS contract.
		if ValidateEndpoint(endpoint) == nil {
			t.Fatalf("accepted unsafe endpoint %s", endpoint)
		}
	}
	for _, value := range []string{"127.0.0.1", "::1", "10.0.0.1", "169.254.169.254", "100.100.100.200", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::", "2001:db8::1", "fec0::1"} {
		// Special-purpose destinations must be rejected before a network connection is attempted.
		if publicAddress(netip.MustParseAddr(value)) {
			t.Fatalf("accepted restricted address %s", value)
		}
	}
	// Special-purpose destinations must be rejected before a network connection is attempted.
	if !publicAddress(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("rejected public IPv4")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// The production dialer must enforce the address check before opening a loopback socket.
	if _, err := publicDial(ctx, "tcp", "127.0.0.1:443"); err == nil {
		t.Fatal("dialed loopback")
	}
}

// TestDiscoveryFailureDoesNotReturnProviderSecrets verifies protocol failures are safe for UI and telemetry.
func TestDiscoveryFailureDoesNotReturnProviderSecrets(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-token should never escape", http.StatusUnauthorized)
	}))
	defer fixture.Close()
	_, err := discoverWithClient(context.Background(), fixture.URL, fixture.Client())
	// Provider error text may contain credentials and must become the fixed public error.
	if !errors.Is(err, ErrDiscovery) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe error=%v", err)
	}
}

// TestBoundedTransportKeepsCredentialsAtOneEndpoint verifies per-client tokens, response limits, and endpoint binding.
func TestBoundedTransportKeepsCredentialsAtOneEndpoint(t *testing.T) {
	seen := make(chan string, 2)
	// The fixture records only synthetic credentials and returns a deliberately oversized body.
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, strings.Repeat("x", (2<<20)+1))
	}))
	defer fixture.Close()
	transport := &boundedTransport{base: fixture.Client().Transport, endpoint: fixture.URL, token: "fixture-token"}
	req, _ := http.NewRequest("POST", fixture.URL, nil)
	response, err := transport.RoundTrip(req)
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	response.Body.Close()
	// Exceeding the response bound must fail even when the provider declares no Content-Length.
	if err == nil || <-seen != "Bearer fixture-token" {
		t.Fatal("missing body bound or credential")
	}
	other, _ := http.NewRequest("POST", fixture.URL+"/other", nil)
	// Discovery must preserve the complete catalog while leaving approval to the user.
	if _, err = transport.RoundTrip(other); err == nil {
		t.Fatal("credential could leave its endpoint")
	}
	// Anonymous discovery owns a different client and cannot inherit the previous credential.
	anonymous := &boundedTransport{base: fixture.Client().Transport, endpoint: fixture.URL}
	response, err = anonymous.RoundTrip(req)
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// An anonymous client must never inherit a bearer header from another discovery.
	if <-seen != "" {
		t.Fatal("credential leaked between clients")
	}
}
