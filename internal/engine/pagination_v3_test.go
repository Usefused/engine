package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestPaginationV3ComposableStrategies(t *testing.T) {
	tests := []struct {
		name      string
		policy    paginationpolicy.Config
		object    *models.IntegrationObject
		respond   func(*testing.T, *http.Request, int) (string, http.Header)
		wantCalls int
		wantItems []int
		itemsPath string
	}{
		v3TokenCase(),
		v3OffsetCase(),
		v3RFCLinkCase(),
		v3HybridCase(),
		v3BareArrayCase(),
		v3ConditionalItemsCase(),
		v3GraphQLCase(),
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
				body, headers := test.respond(t, request, calls)
				calls++
				return paginationResponse(request, body, headers), nil
			})}}
			test.object.Pagination = modelPolicy(test.policy)
			stream := &mockStream{}
			status, err := dispatcher.ExecuteStream(context.Background(), &models.Service{BaseURL: "https://provider.test"}, explicitAnonymousEndpoint(test.object), nil, nil, nil, stream)
			if err != nil || status != http.StatusOK {
				t.Fatalf("ExecuteStream status=%d err=%v", status, err)
			}
			if calls != test.wantCalls || len(stream.chunks) != 1 {
				t.Fatalf("provider calls=%d chunks=%d", calls, len(stream.chunks))
			}
			if len(stream.contracts) != 1 || stream.contracts[0] != (responseContractSignal{status: http.StatusOK, family: "json"}) || stream.bodyBeforeContract {
				t.Fatalf("response contract=%#v bodyBeforeContract=%t", stream.contracts, stream.bodyBeforeContract)
			}
			assertV3Items(t, stream.chunks[0], test.itemsPath, test.wantItems)
		})
	}
}

func v3TokenCase() struct {
	name      string
	policy    paginationpolicy.Config
	object    *models.IntegrationObject
	respond   func(*testing.T, *http.Request, int) (string, http.Header)
	wantCalls int
	wantItems []int
	itemsPath string
} {
	initial := ""
	policy := baseV3Policy("$.items")
	policy.Request = []paginationpolicy.RequestStep{{State: "cursor", Target: v3Target("query", "pageToken"), ValueType: "string", Initial: v3String(initial), Apply: "all"}}
	policy.Response.Values = []paginationpolicy.ResponseValue{{Name: "next", Source: v3BodySource("$.next", "string")}}
	policy.Continuation = []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "next"}}
	policy.Termination.StopOnMissingValues = []string{"next"}
	object := v3Object(http.MethodGet, "/items", models.Parameter{Name: "pageToken", In: "query", Type: "string"})
	respond := func(t *testing.T, request *http.Request, call int) (string, http.Header) {
		want := []string{"", "second"}[call]
		if request.URL.Query().Get("pageToken") != want {
			t.Fatalf("page token=%q want=%q", request.URL.Query().Get("pageToken"), want)
		}
		return []string{`{"items":[1],"next":"second"}`, `{"items":[2]}`}[call], nil
	}
	return struct {
		name      string
		policy    paginationpolicy.Config
		object    *models.IntegrationObject
		respond   func(*testing.T, *http.Request, int) (string, http.Header)
		wantCalls int
		wantItems []int
		itemsPath string
	}{"token", policy, object, respond, 2, []int{1, 2}, "$.items"}
}

func v3OffsetCase() struct {
	name      string
	policy    paginationpolicy.Config
	object    *models.IntegrationObject
	respond   func(*testing.T, *http.Request, int) (string, http.Header)
	wantCalls int
	wantItems []int
	itemsPath string
} {
	policy := baseV3Policy("$")
	policy.Request = []paginationpolicy.RequestStep{
		{State: "offset", Target: v3Target("query", "offset"), ValueType: "integer", Initial: v3Integer(0), Apply: "all"},
		{State: "page_size", Target: v3Target("query", "limit"), ValueType: "integer", Constant: v3Integer(2), Apply: "all"},
	}
	policy.Continuation = []paginationpolicy.ContinuationStep{{Kind: "offset", State: "offset", Increment: &paginationpolicy.Increment{Mode: "items_returned"}}}
	policy.Termination.StopOnShortPage = &paginationpolicy.ShortPageTermination{RequestState: "page_size"}
	object := v3Object(http.MethodGet, "/items", models.Parameter{Name: "offset", In: "query", Type: "integer"}, models.Parameter{Name: "limit", In: "query", Type: "integer"})
	respond := func(t *testing.T, request *http.Request, call int) (string, http.Header) {
		if request.URL.Query().Get("offset") != []string{"0", "2"}[call] || request.URL.Query().Get("limit") != "2" {
			t.Fatalf("unexpected offset query %q", request.URL.RawQuery)
		}
		return []string{"[1,2]", "[3]"}[call], nil
	}
	return struct {
		name      string
		policy    paginationpolicy.Config
		object    *models.IntegrationObject
		respond   func(*testing.T, *http.Request, int) (string, http.Header)
		wantCalls int
		wantItems []int
		itemsPath string
	}{"offset_short_page", policy, object, respond, 2, []int{1, 2, 3}, "$"}
}

func v3RFCLinkCase() struct {
	name      string
	policy    paginationpolicy.Config
	object    *models.IntegrationObject
	respond   func(*testing.T, *http.Request, int) (string, http.Header)
	wantCalls int
	wantItems []int
	itemsPath string
} {
	policy := baseV3Policy("$")
	policy.Response.Values = []paginationpolicy.ResponseValue{{Name: "next", Source: paginationpolicy.ValueSource{Location: "link", Name: "Link", Relation: "next", ValueType: "url"}}}
	policy.Continuation = []paginationpolicy.ContinuationStep{{Kind: "rfc_link", State: "next_url", ResponseValue: "next", Origin: &paginationpolicy.OriginPolicy{Mode: "allowlist", AllowedOrigins: []string{"https://provider.test"}}}}
	policy.Termination.StopOnMissingValues = []string{"next"}
	respond := func(t *testing.T, request *http.Request, call int) (string, http.Header) {
		if call == 1 && request.URL.Query().Get("page") != "2" {
			t.Fatalf("RFC Link was not followed: %s", request.URL)
		}
		if call == 0 {
			return "[1]", http.Header{"Link": {`<https://provider.test/items?page=2>; rel="next"`}}
		}
		return "[2]", nil
	}
	return struct {
		name      string
		policy    paginationpolicy.Config
		object    *models.IntegrationObject
		respond   func(*testing.T, *http.Request, int) (string, http.Header)
		wantCalls int
		wantItems []int
		itemsPath string
	}{"rfc_link", policy, v3Object(http.MethodGet, "/items"), respond, 2, []int{1, 2}, "$"}
}

func v3HybridCase() struct {
	name      string
	policy    paginationpolicy.Config
	object    *models.IntegrationObject
	respond   func(*testing.T, *http.Request, int) (string, http.Header)
	wantCalls int
	wantItems []int
	itemsPath string
} {
	policy := baseV3Policy("$.values")
	policy.Request = []paginationpolicy.RequestStep{{State: "page", Target: v3Target("query", "page"), ValueType: "integer", Initial: v3Integer(1), Apply: "all"}}
	policy.Response.Values = []paginationpolicy.ResponseValue{{Name: "next", Source: v3BodySource("$.next", "url")}}
	policy.Continuation = []paginationpolicy.ContinuationStep{
		{Kind: "page", State: "page", Increment: &paginationpolicy.Increment{Mode: "fixed", Value: 1}},
		{Kind: "next_url", State: "next_url", ResponseValue: "next", Origin: &paginationpolicy.OriginPolicy{Mode: "same_origin"}},
	}
	policy.Termination.StopOnMissingValues = []string{"next"}
	respond := func(t *testing.T, request *http.Request, call int) (string, http.Header) {
		if request.URL.Query().Get("page") != []string{"1", "2"}[call] {
			t.Fatalf("hybrid page not composed: %s", request.URL)
		}
		if call == 1 && request.URL.Query().Get("provider_cursor") != "x" {
			t.Fatalf("provider cursor was lost during hybrid composition: %s", request.URL)
		}
		return []string{`{"values":[1],"next":"/items?provider_cursor=x"}`, `{"values":[2]}`}[call], nil
	}
	return struct {
		name      string
		policy    paginationpolicy.Config
		object    *models.IntegrationObject
		respond   func(*testing.T, *http.Request, int) (string, http.Header)
		wantCalls int
		wantItems []int
		itemsPath string
	}{"hybrid", policy, v3Object(http.MethodGet, "/items", models.Parameter{Name: "page", In: "query", Type: "integer"}), respond, 2, []int{1, 2}, "$.values"}
}

func v3BareArrayCase() struct {
	name      string
	policy    paginationpolicy.Config
	object    *models.IntegrationObject
	respond   func(*testing.T, *http.Request, int) (string, http.Header)
	wantCalls int
	wantItems []int
	itemsPath string
} {
	policy := baseV3Policy("$")
	policy.Request = []paginationpolicy.RequestStep{{State: "cursor", Target: v3Target("query", "after"), ValueType: "integer", Apply: "subsequent"}}
	policy.Response.Values = []paginationpolicy.ResponseValue{{Name: "last_id", Source: paginationpolicy.ValueSource{Location: "items", ValueType: "integer", Item: &paginationpolicy.ItemSelector{Position: "last", Path: "$.id"}}}}
	policy.Continuation = []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "last_id"}}
	policy.Termination.StopOnMissingValues = []string{"last_id"}
	respond := func(t *testing.T, request *http.Request, call int) (string, http.Header) {
		if call == 1 && request.URL.Query().Get("after") != "2" {
			t.Fatalf("last item cursor missing: %s", request.URL)
		}
		return []string{`[{"id":1},{"id":2}]`, `[]`}[call], nil
	}
	return struct {
		name      string
		policy    paginationpolicy.Config
		object    *models.IntegrationObject
		respond   func(*testing.T, *http.Request, int) (string, http.Header)
		wantCalls int
		wantItems []int
		itemsPath string
	}{"bare_array_last_item", policy, v3Object(http.MethodGet, "/items", models.Parameter{Name: "after", In: "query", Type: "integer"}), respond, 2, []int{1, 2}, "$"}
}

func v3ConditionalItemsCase() struct {
	name      string
	policy    paginationpolicy.Config
	object    *models.IntegrationObject
	respond   func(*testing.T, *http.Request, int) (string, http.Header)
	wantCalls int
	wantItems []int
	itemsPath string
} {
	policy := baseV3Policy("")
	policy.Request = []paginationpolicy.RequestStep{
		{State: "entries", Target: v3Target("query", "entries"), ValueType: "boolean", Constant: v3Boolean(true), Apply: "all"},
		{State: "cursor", Target: v3Target("query", "cursor"), ValueType: "string", Apply: "subsequent"},
	}
	policy.Response.Items.Paths = []paginationpolicy.ConditionalPath{{Path: "$.entries", When: paginationpolicy.RequestCondition{State: "entries", Operator: "equals", Value: v3Boolean(true)}}, {Path: "$.orders", When: paginationpolicy.RequestCondition{State: "entries", Operator: "equals", Value: v3Boolean(false)}}}
	policy.Response.Values = []paginationpolicy.ResponseValue{{Name: "next", Source: v3BodySource("$.next", "string")}}
	policy.Continuation = []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "next"}}
	policy.Termination.StopOnMissingValues = []string{"next"}
	respond := func(t *testing.T, request *http.Request, call int) (string, http.Header) {
		if request.URL.Query().Get("entries") != "true" {
			t.Fatalf("constant request state not applied: %s", request.URL)
		}
		return []string{`{"entries":[1],"next":"c2"}`, `{"entries":[2]}`}[call], nil
	}
	object := v3Object(http.MethodGet, "/items", models.Parameter{Name: "entries", In: "query", Type: "boolean"}, models.Parameter{Name: "cursor", In: "query", Type: "string"})
	return struct {
		name      string
		policy    paginationpolicy.Config
		object    *models.IntegrationObject
		respond   func(*testing.T, *http.Request, int) (string, http.Header)
		wantCalls int
		wantItems []int
		itemsPath string
	}{"conditional_items", policy, object, respond, 2, []int{1, 2}, "$.entries"}
}

func v3GraphQLCase() struct {
	name      string
	policy    paginationpolicy.Config
	object    *models.IntegrationObject
	respond   func(*testing.T, *http.Request, int) (string, http.Header)
	wantCalls int
	wantItems []int
	itemsPath string
} {
	policy := baseV3Policy("$.data.items.nodes")
	policy.Request = []paginationpolicy.RequestStep{{State: "cursor", Target: v3Target("graphql_variable", "after"), ValueType: "string", Apply: "subsequent"}}
	policy.Response.Values = []paginationpolicy.ResponseValue{{Name: "end_cursor", Source: paginationpolicy.ValueSource{Location: "graphql", Path: "$.data.items.pageInfo.endCursor", ValueType: "string"}}}
	policy.Continuation = []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "end_cursor"}}
	policy.Termination.StopOnMissingValues = []string{"end_cursor"}
	policy.GraphQL = &paginationpolicy.GraphQLPlan{
		Variables:              []paginationpolicy.GraphQLVariable{{Name: "after", State: "cursor", ValueType: "string"}},
		ResultAliases:          []paginationpolicy.GraphQLResultAlias{{Name: "items", Alias: "repositories"}},
		FirstPageTemplate:      "query First { repositories { nodes { id } pageInfo { endCursor } } }",
		SubsequentPageTemplate: "query Next($after: String!) { repositories(after: $after) { nodes { id } pageInfo { endCursor } } }",
	}
	respond := func(t *testing.T, request *http.Request, call int) (string, http.Header) {
		var envelope struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if call == 0 && !strings.Contains(envelope.Query, "query First") || call == 1 && (!strings.Contains(envelope.Query, "query Next") || envelope.Variables["after"] != "c2") {
			t.Fatalf("unexpected GraphQL page envelope: %+v", envelope)
		}
		return []string{`{"data":{"repositories":{"nodes":[1],"pageInfo":{"endCursor":"c2"}}}}`, `{"data":{"repositories":{"nodes":[2],"pageInfo":{}}}}`}[call], nil
	}
	query := policy.GraphQL.FirstPageTemplate
	object := v3Object(http.MethodPost, "/graphql")
	object.GraphQLQuery, object.ProviderProtocol, object.OperationKind = &query, models.ProviderProtocolGraphQL, models.OperationKindQuery
	return struct {
		name      string
		policy    paginationpolicy.Config
		object    *models.IntegrationObject
		respond   func(*testing.T, *http.Request, int) (string, http.Header)
		wantCalls int
		wantItems []int
		itemsPath string
	}{"graphql_templates_aliases", policy, object, respond, 2, []int{1, 2}, "$.data.repositories.nodes"}
}

func TestPaginationV3RepeatedValuesAndBoundsFailSafely(t *testing.T) {
	tests := []struct {
		name      string
		repeated  paginationpolicy.RepeatedValueBehavior
		limits    paginationpolicy.Limits
		body      string
		wantCode  string
		wantCalls int
	}{
		{name: "repeated_stop", repeated: "stop", body: `{"items":[1],"next":"initial"}`, wantCalls: 1},
		{name: "repeated_error", repeated: "error", body: `{"items":[1],"next":"initial"}`, wantCode: "cycle", wantCalls: 1},
		{name: "max_pages", repeated: "error", limits: paginationpolicy.Limits{MaxPages: 1, MaxItems: 10, MaxBytes: 1024, MaxDurationMs: 1000}, body: `{"items":[1],"next":"new"}`, wantCode: "max_pages", wantCalls: 1},
		{name: "max_items", repeated: "error", limits: paginationpolicy.Limits{MaxPages: 2, MaxItems: 1, MaxBytes: 1024, MaxDurationMs: 1000}, body: `{"items":[1,2],"next":"new"}`, wantCode: "max_items", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initial := "initial"
			policy := baseV3Policy("$.items")
			policy.Request = []paginationpolicy.RequestStep{{State: "cursor", Target: v3Target("query", "cursor"), ValueType: "string", Initial: v3String(initial), Apply: "all"}}
			policy.Response.Values = []paginationpolicy.ResponseValue{{Name: "next", Source: v3BodySource("$.next", "string")}}
			policy.Continuation = []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "next"}}
			policy.Termination.RepeatedValue = test.repeated
			if test.limits.MaxPages != 0 {
				policy.Limits = test.limits
			}
			calls := 0
			dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				return paginationResponse(request, test.body, nil), nil
			})}}
			object := v3Object(http.MethodGet, "/items", models.Parameter{Name: "cursor", In: "query", Type: "string"})
			object.Pagination = modelPolicy(policy)
			_, err := dispatcher.ExecuteStream(context.Background(), &models.Service{BaseURL: "https://provider.test"}, explicitAnonymousEndpoint(object), nil, nil, nil, &mockStream{})
			if PaginationFailureCode(err) != test.wantCode || calls != test.wantCalls {
				t.Fatalf("code=%q calls=%d err=%v", PaginationFailureCode(err), calls, err)
			}
		})
	}
}

func TestPaginationV3RejectsTargetTypeBeforeProviderDispatch(t *testing.T) {
	caseData := v3TokenCase()
	caseData.object.Parameters[0].Type = "integer"
	caseData.object.Pagination = modelPolicy(caseData.policy)
	calls := 0
	dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		return paginationResponse(request, `{}`, nil), nil
	})}}
	_, err := dispatcher.ExecuteStream(context.Background(), &models.Service{BaseURL: "https://provider.test"}, explicitAnonymousEndpoint(caseData.object), nil, nil, nil, &mockStream{})
	if PaginationFailureCode(err) != "request_target_invalid" || calls != 0 {
		t.Fatalf("error=%v provider calls=%d", err, calls)
	}
}

func TestPaginationV3AccumulatesPageTimingsAndAuditSummary(t *testing.T) {
	rateStore := &providerRateLimitStoreStub{}
	caseData := v3TokenCase()
	calls := 0
	dispatcher := &Dispatcher{rateLimits: rateStore, client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		body, headers := caseData.respond(t, request, calls)
		calls++
		return paginationResponse(request, body, headers), nil
	})}}
	caseData.object.Pagination = modelPolicy(caseData.policy)
	timings := NewExecutionTimings()
	ctx := ContextWithExecutionTimings(context.Background(), timings)
	ctx = WithProviderRateLimitIdentity(ctx, uuid.New(), uuid.New(), uuid.Nil)
	srv := &models.Service{BaseURL: "https://provider.test", ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5)}
	status, err := dispatcher.ExecuteStream(ctx, srv, explicitAnonymousEndpoint(caseData.object), nil, nil, nil, &mockStream{})
	if err != nil || status != http.StatusOK || calls != 2 || len(rateStore.requests) != 2 {
		t.Fatalf("status=%d err=%v calls=%d acquisitions=%d", status, err, calls, len(rateStore.requests))
	}
	snapshot := timings.SnapshotMilliseconds()
	summary := timings.PaginationSummary()
	_, hasProviderTotal := snapshot["provider_total"]
	_, hasRateLimitAcquire := snapshot["rate_limit_acquire_ms"]
	if !hasProviderTotal || !hasRateLimitAcquire || summary.Type != "composable" || summary.PageCount != 2 || summary.ItemCount != 2 {
		t.Fatalf("timings=%#v pagination=%+v", snapshot, summary)
	}
}

func TestPaginationV3TelemetryUsesOnlyBoundedStrategyMetadata(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	caseData := v3HybridCase()
	caseData.object.Pagination = modelPolicy(caseData.policy)
	calls := 0
	dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		body := `{"values":[1],"next":"/items?provider_cursor=secret-token"}`
		if calls == 2 {
			body = `{"values":[2]}`
		}
		return paginationResponse(request, body, nil), nil
	})}}
	_, err := dispatcher.ExecuteStream(context.Background(), &models.Service{BaseURL: "https://provider.test"}, explicitAnonymousEndpoint(caseData.object), nil, nil, nil, &mockStream{})
	if err != nil {
		t.Fatal(err)
	}
	attributes := paginationV3SpanAttributes(t, recorder.Ended())
	if attributes["pagination.continuation_kinds"] != `["next_url","page"]` || attributes["pagination.continuation_step_count"] != "2" {
		t.Fatalf("bounded continuation telemetry = %#v", attributes)
	}
	for key, value := range attributes {
		if strings.Contains(key+value, "secret-token") || strings.Contains(key+value, "provider_cursor") || strings.Contains(key+value, "/items") {
			t.Fatalf("pagination telemetry leaked provider data: %s=%s", key, value)
		}
	}
}

func paginationV3SpanAttributes(t *testing.T, spans []sdktrace.ReadOnlySpan) map[string]string {
	t.Helper()
	for _, span := range spans {
		if span.Name() != "engine.dispatch.paginate" {
			continue
		}
		result := make(map[string]string, len(span.Attributes()))
		for _, item := range span.Attributes() {
			result[string(item.Key)] = item.Value.Emit()
		}
		return result
	}
	t.Fatal("pagination span was not recorded")
	return nil
}

func baseV3Policy(itemsPath string) paginationpolicy.Config {
	return paginationpolicy.Config{
		Version:     paginationpolicy.Version,
		Response:    paginationpolicy.ResponsePlan{Items: paginationpolicy.ItemsSource{Path: itemsPath}},
		Termination: paginationpolicy.Termination{StopOnEmptyItems: true, RepeatedValue: "error"},
		Limits:      paginationpolicy.Limits{MaxPages: 10, MaxItems: 100, MaxBytes: 1 << 20, MaxDurationMs: 5_000},
	}
}

func v3Object(method, path string, parameters ...models.Parameter) *models.IntegrationObject {
	return &models.IntegrationObject{Path: path, Method: method, Parameters: parameters, StableKey: "rest:" + method + ":" + path}
}

func v3Target(location paginationpolicy.RequestLocation, name string) paginationpolicy.RequestTarget {
	return paginationpolicy.RequestTarget{Location: location, Name: name}
}

func v3BodySource(path string, valueType paginationpolicy.ValueType) paginationpolicy.ValueSource {
	return paginationpolicy.ValueSource{Location: "body", Path: path, ValueType: valueType}
}

func v3String(value string) *paginationpolicy.Scalar {
	return &paginationpolicy.Scalar{Type: "string", String: &value}
}
func v3Integer(value int64) *paginationpolicy.Scalar {
	return &paginationpolicy.Scalar{Type: "integer", Integer: &value}
}
func v3Boolean(value bool) *paginationpolicy.Scalar {
	return &paginationpolicy.Scalar{Type: "boolean", Boolean: &value}
}

func assertV3Items(t *testing.T, payload []byte, path string, want []int) {
	t.Helper()
	var document any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	items, found := valueAtPath(document, path)
	list, ok := items.([]any)
	if !found || !ok || len(list) != len(want) {
		t.Fatalf("items=%v want=%v", items, want)
	}
	for i := range want {
		item := list[i]
		if object, ok := item.(map[string]any); ok {
			item = object["id"]
		}
		value, err := convertSourceValue(item, "integer")
		if err != nil || value.(int64) != int64(want[i]) {
			t.Fatalf("items=%v want=%v", items, want)
		}
	}
}
