package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type paginationV3State struct {
	values     map[string]any
	visited    map[string]map[string]struct{}
	nextURL    *url.URL
	pages      int64
	items      int64
	bytes      int64
	itemsPath  string
	stopReason string
}

type paginationV3Response struct {
	document  any
	itemsPath string
	itemCount int64
	values    map[string]any
	present   map[string]bool
}

// runPaginationV3 owns the complete provider loop so generated clients observe
// one logical execution and one bounded audit regardless of page count.
func (d *Dispatcher) runPaginationV3(ctx context.Context, span trace.Span, srv *models.Service, obj *models.IntegrationObject, params, credentials map[string]any, bucketValues []store.BucketValue, stream ResponseStream, policy *paginationpolicy.Config) (status int, err error) {
	if err := validatePaginationV3Targets(obj, policy); err != nil {
		return 0, err
	}
	limits := paginationpolicy.EffectiveLimits(policy.Limits)
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.MaxDurationMs)*time.Millisecond)
	defer cancel()
	state := newPaginationV3State(policy)
	defer func() { recordPaginationV3Outcome(ctx, span, state, policy, err) }()
	return d.executePaginationV3Pages(ctx, runCtx, srv, obj, cloneParams(params), credentials, bucketValues, stream, policy, limits, state)
}

func (d *Dispatcher) executePaginationV3Pages(ctx, runCtx context.Context, srv *models.Service, obj *models.IntegrationObject, baseParams, credentials map[string]any, bucketValues []store.BucketValue, stream ResponseStream, policy *paginationpolicy.Config, limits paginationpolicy.Limits, state *paginationV3State) (status int, err error) {
	aggregate := &paginationAggregate{}
	for {
		var response paginationV3Response
		var page *paginationPage
		status, response, page, err = d.executePaginationV3Page(ctx, runCtx, srv, obj, baseParams, credentials, bucketValues, policy, limits, state, aggregate)
		if err != nil {
			return status, err
		}
		stop, err := finishPaginationV3Page(policy, state, response, page)
		if err != nil {
			return status, err
		}
		if stop {
			return status, sendPaginationResult(stream, status, aggregate, state.itemsPath, limits.MaxBytes)
		}
	}
}

func (d *Dispatcher) executePaginationV3Page(ctx, runCtx context.Context, srv *models.Service, obj *models.IntegrationObject, baseParams, credentials map[string]any, bucketValues []store.BucketValue, policy *paginationpolicy.Config, limits paginationpolicy.Limits, state *paginationV3State, aggregate *paginationAggregate) (int, paginationV3Response, *paginationPage, error) {
	if err := checkPaginationV3Bounds(ctx, runCtx, state, limits); err != nil {
		return 0, paginationV3Response{}, nil, err
	}
	pageObj, pageParams := paginationV3Request(obj, baseParams, policy, state)
	page := &paginationPage{maxBytes: int64(limits.MaxBytes) - state.bytes, headers: make(http.Header), headerKeys: paginationV3HeaderKeys(policy)}
	status, err := d.executeWithRetries(runCtx, srv, pageObj, pageParams, credentials, bucketValues, page)
	if err != nil {
		return status, paginationV3Response{}, page, paginationProviderError(ctx, err)
	}
	response, err := decodePaginationV3Page(page, policy, state)
	if err != nil {
		return status, response, page, err
	}
	if state.items+response.itemCount > int64(limits.MaxItems) {
		return status, response, page, paginationError("max_items")
	}
	if state.itemsPath == "" {
		state.itemsPath = response.itemsPath
	}
	if err := aggregate.Add(response.document, response.itemsPath); err != nil {
		return status, response, page, err
	}
	recordPaginationV3Page(ctx, state, response.itemCount, int64(page.body.Len()))
	return status, response, page, nil
}

func recordPaginationV3Page(ctx context.Context, state *paginationV3State, items, bytes int64) {
	state.pages++
	state.items += items
	state.bytes += bytes
	observability.PaginationTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("type", "composable")))
}

func finishPaginationV3Page(policy *paginationpolicy.Config, state *paginationV3State, response paginationV3Response, page *paginationPage) (bool, error) {
	stop, err := terminatePaginationV3(policy, state, response)
	if err != nil || stop {
		return stop, err
	}
	return advancePaginationV3(policy, state, response, page)
}

func checkPaginationV3Bounds(parent, run context.Context, state *paginationV3State, limits paginationpolicy.Limits) error {
	if err := checkPaginationContext(parent, run); err != nil {
		return err
	}
	if state.pages >= int64(limits.MaxPages) {
		return paginationError("max_pages")
	}
	return nil
}

func newPaginationV3State(policy *paginationpolicy.Config) *paginationV3State {
	state := &paginationV3State{values: make(map[string]any), visited: make(map[string]map[string]struct{})}
	for _, step := range policy.Request {
		if step.State != "" && step.Initial != nil {
			state.values[step.State] = paginationScalarValue(*step.Initial)
		}
		if step.State != "" && step.Constant != nil {
			// Constants are state too: conditional response paths and short-page
			// termination must see the exact value written to every request.
			state.values[step.State] = paginationScalarValue(*step.Constant)
		}
	}
	return state
}

func paginationV3Request(obj *models.IntegrationObject, base map[string]any, policy *paginationpolicy.Config, state *paginationV3State) (*models.IntegrationObject, map[string]any) {
	pageObj := *obj
	pageObj.Parameters = append(models.Parameters(nil), obj.Parameters...)
	params := cloneParams(base)
	if state.nextURL != nil {
		pageObj.Path = state.nextURL.String()
		params = headerParamsOnly(params, obj.Parameters)
	}
	applyPaginationGraphQLTemplate(&pageObj, policy.GraphQL, state.pages)
	for _, step := range policy.Request {
		if requestStepApplies(step.Apply, state.pages) {
			applyPaginationV3RequestStep(&pageObj, params, step, state)
		}
	}
	applyPaginationGraphQLVariables(params, policy.GraphQL, state)
	return &pageObj, params
}

func requestStepApplies(application paginationpolicy.PageApplication, page int64) bool {
	return application == paginationpolicy.ApplyAll || (application == paginationpolicy.ApplyFirst && page == 0) || (application == paginationpolicy.ApplySubsequent && page > 0)
}

func applyPaginationV3RequestStep(obj *models.IntegrationObject, params map[string]any, step paginationpolicy.RequestStep, state *paginationV3State) {
	value, ok := state.values[step.State]
	if step.Constant != nil {
		value, ok = paginationScalarValue(*step.Constant), true
	}
	if !ok {
		return
	}
	target := step.Target
	if target.Location == paginationpolicy.RequestGraphQLVariable {
		target.Location = paginationpolicy.RequestBody
	}
	injectPaginationTarget(obj, params, target, value)
}

func applyPaginationGraphQLTemplate(obj *models.IntegrationObject, plan *paginationpolicy.GraphQLPlan, page int64) {
	if plan == nil {
		return
	}
	template := plan.SubsequentPageTemplate
	if page == 0 {
		template = plan.FirstPageTemplate
	}
	obj.GraphQLQuery = &template
}

func applyPaginationGraphQLVariables(params map[string]any, plan *paginationpolicy.GraphQLPlan, state *paginationV3State) {
	if plan == nil {
		return
	}
	for _, variable := range plan.Variables {
		if value, ok := state.values[variable.State]; ok {
			params[variable.Name] = value
		}
	}
}

func decodePaginationV3Page(page *paginationPage, policy *paginationpolicy.Config, state *paginationV3State) (paginationV3Response, error) {
	document, err := decodePaginationDocument(page.body.Bytes())
	if err != nil {
		return paginationV3Response{}, err
	}
	itemsPath, ok := selectItemsPath(policy.Response.Items, state.values)
	if !ok {
		return paginationV3Response{}, paginationError("response_invalid")
	}
	// Registry paths name provider fields; aliases describe the actual response
	// keys, so items and scalar extraction must use the same rewrite.
	itemsPath = applyGraphQLResultAliases(itemsPath, policy.GraphQL)
	items, found := valueAtPath(document, itemsPath)
	list, valid := items.([]any)
	if !found || !valid {
		return paginationV3Response{}, paginationError("response_invalid")
	}
	values, present, err := readPaginationV3Values(policy, state, document, list, page)
	if err != nil {
		return paginationV3Response{}, err
	}
	return paginationV3Response{document: document, itemsPath: itemsPath, itemCount: int64(len(list)), values: values, present: present}, nil
}

func decodePaginationDocument(payload []byte) (any, error) {
	var document any
	decoder := jsonDecoder(payload)
	if err := decoder.Decode(&document); err != nil {
		return nil, paginationError("response_invalid")
	}
	return document, nil
}

func readPaginationV3Values(policy *paginationpolicy.Config, state *paginationV3State, document any, items []any, page *paginationPage) (map[string]any, map[string]bool, error) {
	values := make(map[string]any, len(policy.Response.Values))
	present := make(map[string]bool, len(policy.Response.Values))
	for _, response := range policy.Response.Values {
		value, found, err := readPaginationV3Source(response.Source, policy.GraphQL, state.values, document, items, page)
		if err != nil {
			return nil, nil, err
		}
		present[response.Name] = found
		if found {
			values[response.Name] = value
		}
	}
	return values, present, nil
}

func readPaginationV3Source(source paginationpolicy.ValueSource, graphQL *paginationpolicy.GraphQLPlan, state map[string]any, document any, items []any, page *paginationPage) (any, bool, error) {
	switch source.Location {
	case paginationpolicy.SourceHeader:
		value, found := headerValue(page.headers, source.Name)
		return convertLocatedValue(value, found, source.ValueType)
	case paginationpolicy.SourceLink:
		value, found := linkValue(page.headers.Values(source.Name), source.Relation)
		return convertLocatedValue(value, found, source.ValueType)
	case paginationpolicy.SourceItems:
		return readPaginationItemSource(source, items)
	case paginationpolicy.SourceBody, paginationpolicy.SourceGraphQL:
		path, ok := selectSourcePath(source.Path, source.Paths, state)
		if !ok {
			return nil, false, nil
		}
		if source.Location == paginationpolicy.SourceGraphQL {
			path = applyGraphQLResultAliases(path, graphQL)
		}
		value, found := valueAtPath(document, path)
		return convertLocatedValue(value, found, source.ValueType)
	default:
		return nil, false, paginationError("invalid_config")
	}
}

func convertLocatedValue(value any, found bool, valueType paginationpolicy.ValueType) (any, bool, error) {
	if !found || value == nil {
		return nil, false, nil
	}
	converted, err := convertSourceValue(value, string(valueType))
	return converted, err == nil, err
}

func readPaginationItemSource(source paginationpolicy.ValueSource, items []any) (any, bool, error) {
	if len(items) == 0 || source.Item == nil {
		return nil, false, nil
	}
	value := items[len(items)-1]
	if source.Item.Path != "" {
		var found bool
		value, found = valueAtPath(value, source.Item.Path)
		if !found {
			return nil, false, nil
		}
	}
	return convertLocatedValue(value, true, source.ValueType)
}

func selectItemsPath(source paginationpolicy.ItemsSource, state map[string]any) (string, bool) {
	if source.Path != "" {
		return source.Path, true
	}
	for _, candidate := range source.Paths {
		if requestConditionMatches(candidate.When, state) {
			return candidate.Path, true
		}
	}
	return "", false
}

func selectSourcePath(path string, paths []paginationpolicy.ConditionalPath, state map[string]any) (string, bool) {
	if path != "" {
		return path, true
	}
	for _, candidate := range paths {
		if requestConditionMatches(candidate.When, state) {
			return candidate.Path, true
		}
	}
	return "", false
}

func requestConditionMatches(condition paginationpolicy.RequestCondition, state map[string]any) bool {
	actual, present := state[condition.State]
	switch condition.Operator {
	case paginationpolicy.ConditionPresent:
		return present
	case paginationpolicy.ConditionAbsent:
		return !present
	case paginationpolicy.ConditionEquals:
		return present && scalarKey(actual) == scalarKey(paginationScalarValue(*condition.Value))
	case paginationpolicy.ConditionNotEquals:
		return !present || scalarKey(actual) != scalarKey(paginationScalarValue(*condition.Value))
	default:
		return false
	}
}

// terminatePaginationV3 requires a reviewed signal or a hard limit; provider
// response shape alone must not create an unbounded continuation loop.
func terminatePaginationV3(policy *paginationpolicy.Config, state *paginationV3State, response paginationV3Response) (bool, error) {
	if policy.Termination.StopOnEmptyItems && response.itemCount == 0 {
		state.stopReason = "empty_page"
		return true, nil
	}
	if shortPaginationV3Page(policy.Termination.StopOnShortPage, state.values, response.itemCount) {
		state.stopReason = "short_page"
		return true, nil
	}
	for _, name := range policy.Termination.StopOnMissingValues {
		if !response.present[name] {
			state.stopReason = "missing_next"
			return true, nil
		}
	}
	for _, condition := range policy.Termination.Conditions {
		if responseConditionMatches(condition, state.values, response.values, response.present) {
			state.stopReason = "condition"
			return true, nil
		}
	}
	return false, nil
}

func shortPaginationV3Page(stop *paginationpolicy.ShortPageTermination, state map[string]any, itemCount int64) bool {
	if stop == nil {
		return false
	}
	size, ok := state[stop.RequestState].(int64)
	return ok && itemCount < size
}

func responseConditionMatches(condition paginationpolicy.ResponseCondition, state, values map[string]any, present map[string]bool) bool {
	actual, exists := values[condition.ResponseValue]
	if condition.Operator == paginationpolicy.ConditionStateGTE {
		current, currentOK := state[condition.State].(int64)
		total, totalOK := actual.(int64)
		return exists && currentOK && totalOK && current >= total
	}
	return scalarConditionMatches(condition.Operator, actual, exists && present[condition.ResponseValue], condition.Value)
}

func scalarConditionMatches(operator paginationpolicy.ConditionOperator, actual any, present bool, expected *paginationpolicy.Scalar) bool {
	switch operator {
	case paginationpolicy.ConditionPresent:
		return present
	case paginationpolicy.ConditionAbsent:
		return !present
	case paginationpolicy.ConditionEquals:
		return present && scalarKey(actual) == scalarKey(paginationScalarValue(*expected))
	case paginationpolicy.ConditionNotEquals:
		return !present || scalarKey(actual) != scalarKey(paginationScalarValue(*expected))
	default:
		return false
	}
}

func advancePaginationV3(policy *paginationpolicy.Config, state *paginationV3State, response paginationV3Response, page *paginationPage) (bool, error) {
	for _, step := range policy.Continuation {
		next, err := paginationV3ContinuationValue(step, state, response, page)
		if err != nil {
			return false, err
		}
		repeated := paginationV3ValueRepeated(state, step.State, next)
		if repeated {
			if policy.Termination.RepeatedValue == paginationpolicy.RepeatedStop {
				state.stopReason = "repeated_value"
				return true, nil
			}
			return false, paginationError("cycle")
		}
		state.values[step.State] = next
		if step.Kind == paginationpolicy.ContinuationRFCLink || step.Kind == paginationpolicy.ContinuationNextURL {
			state.nextURL = next.(*url.URL)
		}
	}
	return false, nil
}

func paginationV3ContinuationValue(step paginationpolicy.ContinuationStep, state *paginationV3State, response paginationV3Response, page *paginationPage) (any, error) {
	if step.ResponseValue != "" {
		value, ok := response.values[step.ResponseValue]
		if !ok {
			return nil, paginationError("response_invalid")
		}
		if step.Kind == paginationpolicy.ContinuationRFCLink || step.Kind == paginationpolicy.ContinuationNextURL {
			return trustedPaginationV3URL(page.requestURL, value.(string), *step.Origin)
		}
		return value, nil
	}
	current, ok := state.values[step.State].(int64)
	if !ok || step.Increment == nil {
		return nil, paginationError("invalid_config")
	}
	if step.Increment.Mode == paginationpolicy.IncrementItemsReturned {
		return current + response.itemCount, nil
	}
	return current + step.Increment.Value, nil
}

func paginationV3ValueRepeated(state *paginationV3State, name string, value any) bool {
	key := paginationV3StateKey(value)
	seen := state.visited[name]
	if seen == nil {
		seen = make(map[string]struct{})
		state.visited[name] = seen
		if current, ok := state.values[name]; ok {
			seen[paginationV3StateKey(current)] = struct{}{}
		}
	}
	if _, exists := seen[key]; exists {
		return true
	}
	seen[key] = struct{}{}
	return false
}

func paginationV3StateKey(value any) string {
	if target, ok := value.(*url.URL); ok {
		return "u:" + target.String()
	}
	return scalarKey(value)
}

// trustedPaginationV3URL binds provider-supplied next links to the reviewed
// origin policy before they can redirect credentials to another host.
func trustedPaginationV3URL(base *url.URL, raw string, policy paginationpolicy.OriginPolicy) (*url.URL, error) {
	resolved, err := resolvePaginationURL(base, raw)
	if err != nil {
		return nil, err
	}
	origin := paginationURLOrigin(resolved)
	if policy.Mode == paginationpolicy.OriginSame && origin == paginationURLOrigin(base) {
		return resolved, nil
	}
	if policy.Mode == paginationpolicy.OriginList && stringInSlice(policy.AllowedOrigins, origin) {
		return resolved, nil
	}
	return nil, paginationError("untrusted_next_url")
}

func resolvePaginationURL(base *url.URL, raw string) (*url.URL, error) {
	if base == nil {
		return nil, paginationError("response_invalid")
	}
	next, err := url.Parse(raw)
	if err != nil || next.User != nil || next.Fragment != "" {
		return nil, paginationError("response_invalid")
	}
	resolved := base.ResolveReference(next)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return nil, paginationError("response_invalid")
	}
	return resolved, nil
}

func paginationURLOrigin(value *url.URL) string {
	if value == nil {
		return ""
	}
	host := strings.ToLower(value.Hostname())
	port := value.Port()
	if port == "" {
		port = defaultPaginationPort(value.Scheme)
	}
	return strings.ToLower(value.Scheme) + "://" + net.JoinHostPort(host, port)
}

func defaultPaginationPort(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	return "80"
}

func stringInSlice(values []string, wanted string) bool {
	for _, value := range values {
		parsed, err := url.Parse(value)
		if err == nil && paginationURLOrigin(parsed) == wanted {
			return true
		}
	}
	return false
}

func paginationScalarValue(value paginationpolicy.Scalar) any {
	switch value.Type {
	case paginationpolicy.ValueInteger:
		return *value.Integer
	case paginationpolicy.ValueBoolean:
		return *value.Boolean
	default:
		return *value.String
	}
}

func applyGraphQLResultAliases(path string, plan *paginationpolicy.GraphQLPlan) string {
	if plan == nil {
		return path
	}
	parts := strings.Split(path, ".")
	for index, part := range parts {
		for _, alias := range plan.ResultAliases {
			if part == alias.Name {
				parts[index] = alias.Alias
			}
		}
	}
	return strings.Join(parts, ".")
}

func paginationV3HeaderKeys(policy *paginationpolicy.Config) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, value := range policy.Response.Values {
		if value.Source.Location == paginationpolicy.SourceHeader || value.Source.Location == paginationpolicy.SourceLink {
			keys[http.CanonicalHeaderKey(value.Source.Name)] = struct{}{}
		}
	}
	return keys
}

func validatePaginationV3Targets(obj *models.IntegrationObject, policy *paginationpolicy.Config) error {
	if policy.GraphQL != nil && providerProtocol(obj) != models.ProviderProtocolGraphQL {
		return paginationError("request_target_invalid")
	}
	for _, step := range policy.Request {
		if !paginationV3TargetMatches(obj, policy.GraphQL, step) {
			return paginationError("request_target_invalid")
		}
	}
	return nil
}

// ValidatePaginationV3Targets is shared with snapshot ingestion so an invalid
// reviewed target is rejected before any execution can reach the provider.
func ValidatePaginationV3Targets(obj *models.IntegrationObject, policy *paginationpolicy.Config) error {
	return validatePaginationV3Targets(obj, policy)
}

func paginationV3TargetMatches(obj *models.IntegrationObject, graphQL *paginationpolicy.GraphQLPlan, step paginationpolicy.RequestStep) bool {
	if step.Target.Location == paginationpolicy.RequestGraphQLVariable {
		return graphQLVariableMatches(graphQL, step)
	}
	for _, parameter := range obj.Parameters {
		if parameter.Name == step.Target.Name && parameter.In == string(step.Target.Location) {
			return paginationValueTypeMatchesParameter(step.ValueType, parameter)
		}
	}
	return step.Target.Location == paginationpolicy.RequestBody && paginationRequestBodyPropertyMatches(obj.RequestContent, step.Target.Name, step.ValueType)
}

func graphQLVariableMatches(plan *paginationpolicy.GraphQLPlan, step paginationpolicy.RequestStep) bool {
	if plan == nil {
		return false
	}
	for _, variable := range plan.Variables {
		if variable.Name == step.Target.Name && variable.ValueType == step.ValueType && (step.State == "" || variable.State == step.State) {
			return true
		}
	}
	return false
}

func paginationValueTypeMatchesParameter(valueType paginationpolicy.ValueType, parameter models.Parameter) bool {
	typeName := parameter.Type
	if parameter.Schema != nil && parameter.Schema.Projection.Type != "" {
		typeName = parameter.Schema.Projection.Type
	}
	return paginationTypeNameMatches(valueType, typeName)
}

func paginationRequestBodyPropertyMatches(content *models.RequestContent, name string, valueType paginationpolicy.ValueType) bool {
	if content == nil {
		return false
	}
	for _, representation := range content.Representations {
		if representation.Schema != nil {
			if property, ok := representation.Schema.Projection.Properties[name]; ok && paginationTypeNameMatches(valueType, property.Type) {
				return true
			}
		}
	}
	return false
}

func paginationTypeNameMatches(valueType paginationpolicy.ValueType, typeName string) bool {
	switch valueType {
	case paginationpolicy.ValueInteger:
		return typeName == "integer" || typeName == "number"
	case paginationpolicy.ValueBoolean:
		return typeName == "boolean"
	case paginationpolicy.ValueString, paginationpolicy.ValueURL:
		return typeName == "string"
	default:
		return false
	}
}

func recordPaginationV3Outcome(ctx context.Context, span trace.Span, state *paginationV3State, policy *paginationpolicy.Config, err error) {
	stop := state.stopReason
	if stop == "" && err != nil {
		stop = PaginationFailureCode(err)
	}
	summary := PaginationExecutionSummary{Type: "composable", PageCount: state.pages, ItemCount: state.items, ByteCount: state.bytes, StopReason: stop}
	RecordPaginationSummary(ctx, summary)
	kinds := paginationV3ContinuationKinds(policy)
	span.SetAttributes(attribute.String("pagination.type", summary.Type), attribute.Int64("pagination.page_count", summary.PageCount), attribute.Int64("pagination.item_count", summary.ItemCount), attribute.Int64("pagination.byte_count", summary.ByteCount), attribute.String("pagination.stop_reason", summary.StopReason), attribute.StringSlice("pagination.continuation_kinds", kinds), attribute.Int("pagination.continuation_step_count", len(policy.Continuation)))
	if err != nil {
		span.SetStatus(codes.Error, summary.StopReason)
	}
}

func paginationV3ContinuationKinds(policy *paginationpolicy.Config) []string {
	seen := make(map[string]struct{}, len(policy.Continuation))
	for _, step := range policy.Continuation {
		seen[string(step.Kind)] = struct{}{}
	}
	kinds := make([]string, 0, len(seen))
	for kind := range seen {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func jsonDecoder(payload []byte) *json.Decoder {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	return decoder
}
