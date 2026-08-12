package engine

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

type paginationRoundTripper func(*http.Request) (*http.Response, error)

func (fn paginationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestLegacyPaginationIsRejectedBeforeProviderExecution(t *testing.T) {
	calls := 0
	dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		return paginationResponse(request, `{"items":[]}`, nil), nil
	})}}
	legacy := modelPolicy(paginationpolicy.Config{
		Version: 2,
	})

	_, err := dispatcher.ExecuteStream(
		context.Background(),
		&models.Service{BaseURL: "https://provider.test"},
		explicitAnonymousEndpoint(&models.IntegrationObject{Path: "/items", Method: http.MethodGet, Pagination: legacy}),
		nil, nil, nil, &mockStream{},
	)

	if PaginationFailureCode(err) != "invalid_config" || calls != 0 {
		t.Fatalf("legacy pagination err=%v provider_calls=%d", err, calls)
	}
}

func paginationResponse(request *http.Request, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func modelPolicy(config paginationpolicy.Config) *models.PaginationConfig {
	result := models.PaginationConfig(config)
	return &result
}
