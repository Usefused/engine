package engine

import (
	"context"
	"encoding/json"
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

func TestPaginationStrategiesReturnOneAggregatedJSONResponse(t *testing.T) {
	tests := []struct {
		name   string
		policy *models.PaginationConfig
		handle func(*testing.T, *http.Request, int) (string, http.Header)
		want   []int
	}{
		{
			name: "cursor",
			policy: modelPolicy(paginationpolicy.Config{
				Version: 2, Type: "cursor", ItemsPath: "$.items",
				Cursor: &paginationpolicy.CursorConfig{
					Request: paginationpolicy.RequestTarget{Location: "query", Name: "cursor"},
					Next:    paginationpolicy.ValueSource{Location: "body", Path: "$.next", ValueType: "string"},
				},
			}),
			want: []int{1, 2},
			handle: func(t *testing.T, request *http.Request, call int) (string, http.Header) {
				if call == 0 {
					return `{"items":[1],"next":"second"}`, nil
				}
				if request.URL.Query().Get("cursor") != "second" {
					t.Fatalf("cursor was not injected: %s", request.URL.RawQuery)
				}
				return `{"items":[2]}`, nil
			},
		},
		{
			name: "offset",
			policy: modelPolicy(paginationpolicy.Config{
				Version: 2, Type: "offset", ItemsPath: "$.items",
				Offset: &paginationpolicy.OffsetConfig{
					Request:    paginationpolicy.RequestTarget{Location: "query", Name: "offset"},
					Increment:  paginationpolicy.OffsetIncrement{Mode: "fixed", Value: 2},
					PageSize:   &paginationpolicy.PageSize{Target: paginationpolicy.RequestTarget{Location: "query", Name: "limit"}, Value: 2},
					TotalItems: &paginationpolicy.ValueSource{Location: "body", Path: "$.total", ValueType: "integer"},
				},
			}),
			want: []int{1, 2, 3},
			handle: func(t *testing.T, request *http.Request, call int) (string, http.Header) {
				if request.URL.Query().Get("limit") != "2" {
					t.Fatalf("page size was not injected: %s", request.URL.RawQuery)
				}
				if call == 0 {
					return `{"items":[1,2],"total":3}`, nil
				}
				if request.URL.Query().Get("offset") != "2" {
					t.Fatalf("offset was not advanced: %s", request.URL.RawQuery)
				}
				return `{"items":[3],"total":3}`, nil
			},
		},
		{
			name: "page_number",
			policy: modelPolicy(paginationpolicy.Config{
				Version: 2, Type: "page_number", ItemsPath: "$.items",
				PageNumber: &paginationpolicy.PageNumberConfig{
					Request: paginationpolicy.RequestTarget{Location: "query", Name: "page"}, Start: 1, Increment: 1,
					TotalPages: &paginationpolicy.ValueSource{Location: "body", Path: "$.pages", ValueType: "integer"},
				},
			}),
			want: []int{1, 1},
			handle: func(t *testing.T, request *http.Request, call int) (string, http.Header) {
				wanted := []string{"1", "2"}[call]
				if request.URL.Query().Get("page") != wanted {
					t.Fatalf("page number = %s, want %s", request.URL.Query().Get("page"), wanted)
				}
				return `{"items":[1],"pages":2}`, nil
			},
		},
		{
			name: "next_url_link",
			policy: modelPolicy(paginationpolicy.Config{
				Version: 2, Type: "next_url", ItemsPath: "$.items",
				NextURL: &paginationpolicy.NextURLConfig{Next: paginationpolicy.ValueSource{Location: "link", Name: "Link", Relation: "next", ValueType: "url"}},
			}),
			want: []int{1, 2},
			handle: func(t *testing.T, request *http.Request, call int) (string, http.Header) {
				if call == 0 {
					return `{"items":[1]}`, http.Header{"Link": {`<https://provider.test/items?page=2>; rel="next"; title="a,b", <https://provider.test/items?page=9>; rel="last"`}}
				}
				if request.URL.Query().Get("page") != "2" {
					t.Fatalf("next URL was not followed: %s", request.URL.String())
				}
				return `{"items":[2]}`, nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
				body, headers := test.handle(t, request, calls)
				calls++
				return paginationResponse(request, body, headers), nil
			})}}
			stream := &mockStream{}
			status, err := dispatcher.ExecuteStream(context.Background(), &models.Service{BaseURL: "https://provider.test"}, explicitAnonymousEndpoint(&models.IntegrationObject{Path: "/items", Method: http.MethodGet, Pagination: test.policy}), nil, nil, nil, stream)
			if err != nil || status != http.StatusOK {
				t.Fatalf("ExecuteStream status=%d err=%v", status, err)
			}
			if calls != 2 || len(stream.chunks) != 1 {
				t.Fatalf("calls=%d chunks=%d, want two provider calls and one SDK response", calls, len(stream.chunks))
			}
			var response struct {
				Items []int `json:"items"`
			}
			if err := json.Unmarshal(stream.chunks[0], &response); err != nil {
				t.Fatalf("aggregated response is not valid JSON: %v", err)
			}
			if len(response.Items) != len(test.want) {
				t.Fatalf("aggregated items = %#v, want %#v", response.Items, test.want)
			}
			for i := range test.want {
				if response.Items[i] != test.want[i] {
					t.Fatalf("aggregated items = %#v, want %#v", response.Items, test.want)
				}
			}
		})
	}
}

func TestPaginationRejectsCrossOriginNextURL(t *testing.T) {
	policy := modelPolicy(paginationpolicy.Config{
		Version: 2, Type: "next_url", ItemsPath: "$.items",
		NextURL: &paginationpolicy.NextURLConfig{Next: paginationpolicy.ValueSource{Location: "body", Path: "$.next", ValueType: "url"}},
	})
	dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		return paginationResponse(request, `{"items":[1],"next":"https://attacker.test/steal"}`, nil), nil
	})}}

	_, err := dispatcher.ExecuteStream(context.Background(), &models.Service{BaseURL: "https://provider.test"}, explicitAnonymousEndpoint(&models.IntegrationObject{Path: "/items", Method: http.MethodGet, Pagination: policy}), nil, nil, nil, &mockStream{})
	if PaginationFailureCode(err) != "untrusted_next_url" || strings.Contains(err.Error(), "attacker") {
		t.Fatalf("expected safe typed origin error, got %v", err)
	}
}

func TestPaginationFailsWhenConfiguredStopSignalIsMissing(t *testing.T) {
	policy := modelPolicy(paginationpolicy.Config{
		Version: 2, Type: "offset", ItemsPath: "$.items",
		Offset: &paginationpolicy.OffsetConfig{
			Request:   paginationpolicy.RequestTarget{Location: "query", Name: "offset"},
			Increment: paginationpolicy.OffsetIncrement{Mode: "fixed", Value: 1},
			HasMore:   &paginationpolicy.ValueSource{Location: "body", Path: "$.has_more", ValueType: "boolean"},
		},
	})
	dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		return paginationResponse(request, `{"items":[1]}`, nil), nil
	})}}

	_, err := dispatcher.ExecuteStream(context.Background(), &models.Service{BaseURL: "https://provider.test"}, explicitAnonymousEndpoint(&models.IntegrationObject{Path: "/items", Method: http.MethodGet, Pagination: policy}), nil, nil, nil, &mockStream{})
	if PaginationFailureCode(err) != "response_invalid" {
		t.Fatalf("expected missing configured stop source to fail closed, got %v", err)
	}
}

func TestPaginationDetectsTypedCursorCycleWithoutExposingValue(t *testing.T) {
	initial := "secret-cursor"
	policy := modelPolicy(paginationpolicy.Config{
		Version: 2, Type: "cursor", ItemsPath: "$.items",
		Cursor: &paginationpolicy.CursorConfig{
			Request: paginationpolicy.RequestTarget{Location: "query", Name: "cursor"},
			Initial: &paginationpolicy.Scalar{Type: "string", String: &initial},
			Next:    paginationpolicy.ValueSource{Location: "body", Path: "$.next", ValueType: "string"},
		},
	})
	dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		return paginationResponse(request, `{"items":[1],"next":"secret-cursor"}`, nil), nil
	})}}

	_, err := dispatcher.ExecuteStream(context.Background(), &models.Service{BaseURL: "https://provider.test"}, explicitAnonymousEndpoint(&models.IntegrationObject{Path: "/items", Method: http.MethodGet, Pagination: policy}), nil, nil, nil, &mockStream{})
	if PaginationFailureCode(err) != "cycle" || strings.Contains(err.Error(), initial) {
		t.Fatalf("expected safe typed cycle error, got %v", err)
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
