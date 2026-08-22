package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestProviderConnectionLogOmitsResolvedHost keeps bucket-supplied server
// variables out of structured logs after the provider connection succeeds.
func TestProviderConnectionLogOmitsResolvedHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	// The connection callback exists only on a valid request, so construction failure cannot be ignored.
	if err != nil {
		t.Fatal(err)
	}
	_, span := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "test")
	request = requestWithProviderHTTPTrace(
		context.Background(), request,
		&models.Service{Name: "Sendbird"},
		&models.IntegrationObject{Name: "listUsers", Method: http.MethodGet},
		span,
	)
	response, err := http.DefaultClient.Do(request)
	// A real connection is required to trigger httptrace.GotConn and exercise the log boundary.
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	encoded := logs.String()
	// The selected host can contain a bucket value and must not survive as either a field or message text.
	if strings.Contains(encoded, server.URL) || strings.Contains(encoded, request.URL.Host) || strings.Contains(encoded, `"host"`) {
		t.Fatalf("provider connection log exposed resolved host: %s", encoded)
	}
}

// TestDispatcherTransportErrorOmitsResolvedURL keeps raw Go URL errors behind
// the existing typed transport failure while preserving internal classification.
func TestDispatcherTransportErrorOmitsResolvedURL(t *testing.T) {
	const providerURL = "https://api-tenant-routing-value.sendbird.test"
	dispatcher := NewDispatcher()
	dispatcher.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial " + providerURL + ": certificate rejected")
	})}

	_, err := dispatcher.ExecuteStream(
		context.Background(),
		&models.Service{Name: "Sendbird", BaseURL: providerURL},
		explicitAnonymousEndpoint(&models.IntegrationObject{Name: "listUsers", Path: "/v3/users", Method: http.MethodGet}),
		nil, nil, nil, &mockStream{},
	)
	// A transport failure must remain observable without returning its secret-bearing URL text.
	if err == nil || err.Error() != "provider transport failed" {
		t.Fatalf("transport error = %v", err)
	}
	var transport *providerTransportError
	// Internal retry and audit classifiers still need the typed cause chain after public sanitization.
	if !errors.As(err, &transport) || classifyRetryError(err) != "tls_handshake" && classifyRetryError(err) != "transport" {
		t.Fatalf("transport classification was not retained: %T", err)
	}
	// Neither the complete destination nor its bucket-derived segment may reach the caller.
	if strings.Contains(err.Error(), providerURL) || strings.Contains(err.Error(), "tenant-routing-value") {
		t.Fatalf("transport error exposed resolved provider routing")
	}
}
