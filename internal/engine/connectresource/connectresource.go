package connectresource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

const maxDiscoveryBodyBytes = 2 << 20

var (
	// ErrDiscoveryInputNoMatch keeps an ungranted customer-selected tenant out of routing state.
	ErrDiscoveryInputNoMatch = errors.New("resource input did not match a discovered resource")
	// ErrDiscoveryInputAmbiguous prevents duplicate provider grants from selecting an arbitrary tenant.
	ErrDiscoveryInputAmbiguous = errors.New("resource input matched multiple discovered resources")
)

type Resource struct {
	ProviderID string
	Type       string
	Name       string
	BaseURL    string
	Metadata   []byte
	Scopes     []string
}

// Discover executes the configured provider operation with the fresh access
// token and turns its bounded response into validated routing records.
func Discover(ctx context.Context, metadata *fusedobject.ServiceMetadata, endpoint *fusedobject.Endpoint, token, tokenType string) ([]Resource, error) {
	config := metadata.ConnectConfig.ResourceDiscovery
	// Importers already reject mutating discovery operations; this runtime
	// check protects Engines consuming older or manually-authored metadata.
	if !strings.EqualFold(endpoint.Method, http.MethodGet) {
		return nil, errors.New("resource discovery operation must use GET")
	}
	requestURL, err := resolveOperationURL(discoveryBaseURL(metadata, config.Server), endpoint.Path)
	if err != nil {
		return nil, errors.New("resource discovery endpoint is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, endpoint.Method, requestURL, nil)
	if err != nil {
		return nil, errors.New("resource discovery request is invalid")
	}
	// Registry-owned defaults are safe here; customer input never supplies discovery headers.
	for key, value := range metadata.DefaultHeaders {
		req.Header.Set(key, value)
	}
	req.Header.Set("Authorization", defaultTokenType(tokenType)+" "+token)
	resp, err := discoveryHTTPClient(req.URL).Do(req)
	if err != nil {
		return nil, errors.New("resource discovery request failed")
	}
	defer resp.Body.Close()
	// Only successful provider responses may become trusted routing records.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("resource discovery returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBodyBytes+1))
	// The hard limit bounds memory and prevents partial oversized JSON from being accepted.
	if err != nil || len(body) > maxDiscoveryBodyBytes {
		return nil, errors.New("resource discovery response is invalid")
	}
	return ExtractWithMetadata(body, config, metadata.ConnectConfig.Metadata)
}

// Extract supports the documented JSONPath subset and aligns values by array
// index so IDs, names, URLs, and scopes cannot drift between resources.
func Extract(body []byte, config *fusedobject.ResourceDiscoveryConfig) ([]Resource, error) {
	return ExtractWithMetadata(body, config, nil)
}

// ExtractWithMetadata performs one aligned pass over the bounded discovery
// response so resource IDs and optional context cannot drift between rows.
func ExtractWithMetadata(body []byte, config *fusedobject.ResourceDiscoveryConfig, metadataPaths map[string]string) ([]Resource, error) {
	payload, err := decodeDiscoveryPayload(body)
	if err != nil {
		return nil, errors.New("resource discovery response is not valid JSON")
	}
	ids, err := pathValues(payload, config.IDPath)
	// Resource identity is mandatory because reconciliation keys provider rows by it.
	if err != nil || len(ids) == 0 {
		return nil, errors.New("resource discovery did not return resource IDs")
	}
	resources := make([]Resource, 0, len(ids))
	// Positional iteration keeps optional fields aligned to the same provider resource.
	for index, rawID := range ids {
		id, ok := discoveryScalarString(rawID)
		// Objects, arrays, booleans, null, and empty IDs are unstable identities.
		if !ok || strings.TrimSpace(id) == "" {
			return nil, errors.New("resource discovery IDs must be strings or numbers")
		}
		resource, err := resourceAt(payload, config, metadataPaths, index, id)
		if err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, nil
}

// decodeDiscoveryPayload preserves numeric provider IDs exactly and rejects
// trailing JSON that could hide a second unvalidated document.
func decodeDiscoveryPayload(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("resource discovery response contains trailing JSON")
	}
	return payload, nil
}

// discoveryScalarString accepts stable textual and numeric IDs while refusing
// objects, arrays, booleans, and null as resource identity.
func discoveryScalarString(value any) (string, bool) {
	// JSON numbers remain textual so identifiers above 2^53 avoid float64.
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	default:
		return "", false
	}
}

// FromInput validates pre-authorisation tenant values and derives one resource
// without allowing callers to submit an arbitrary URL directly.
func FromInput(config *fusedobject.ResourceInputConfig, values map[string]string) (Resource, error) {
	normalized, missing, err := NormalizeInput(config, values)
	if err != nil {
		return Resource{}, err
	}
	if len(missing) != 0 {
		return Resource{}, fmt.Errorf("resource input %q is required", missing[0])
	}
	baseURL := renderTemplate(config.BaseURLTemplate, normalized)
	if err := ValidateBaseURL(baseURL, config.AllowedHosts); err != nil {
		return Resource{}, err
	}
	canonical, _ := json.Marshal(normalized)
	sum := sha256.Sum256(canonical)
	return Resource{
		ProviderID: "input:" + hex.EncodeToString(sum[:]),
		Type:       config.ResourceType,
		Name:       firstInputDisplayName(config.Fields, normalized),
		BaseURL:    baseURL,
		Metadata:   canonical,
	}, nil
}

// MatchDiscoveredInput retains the single provider-granted resource whose
// declared metadata URL equals the validated URL derived from customer input.
func MatchDiscoveredInput(config *fusedobject.ResourceInputConfig, values map[string]string, resources []Resource) (Resource, error) {
	// Runtime callers fail closed if unvalidated metadata omitted the explicit match contract.
	if config == nil || config.DiscoveryMatch == nil {
		return Resource{}, errors.New("resource discovery match is not configured")
	}
	expected, err := FromInput(config, values)
	// Invalid or incomplete customer input cannot fall back to provider-only selection.
	if err != nil {
		return Resource{}, err
	}
	expectedURL, err := canonicalDiscoveryMatchURL(expected.BaseURL, config.AllowedHosts)
	// The derived URL must satisfy the same canonical routing boundary used for provider metadata.
	if err != nil {
		return Resource{}, err
	}
	matched, matchCount, err := matchingDiscoveredResource(resources, config.DiscoveryMatch.MetadataKey, expectedURL, config.AllowedHosts)
	// Any malformed grant invalidates the complete provider response.
	if err != nil {
		return Resource{}, err
	}
	// Exactly one grant prevents missing input matches and ambiguous duplicate
	// provider rows from becoming usable routing state.
	if matchCount == 0 {
		return Resource{}, ErrDiscoveryInputNoMatch
	}
	if matchCount > 1 {
		return Resource{}, ErrDiscoveryInputAmbiguous
	}
	return matched, nil
}

// matchingDiscoveredResource counts exact grants without retaining provider
// metadata beyond the single original resource needed by routing.
func matchingDiscoveredResource(resources []Resource, metadataKey, expectedURL string, allowedHosts []string) (Resource, int, error) {
	var matched Resource
	matchCount := 0
	// Every provider row is evaluated once so duplicate matches remain observable.
	for _, resource := range resources {
		candidate, err := discoveryMatchMetadataValue(resource.Metadata, metadataKey)
		// Malformed declared metadata invalidates the grant set instead of being silently ignored.
		if err != nil {
			return Resource{}, 0, err
		}
		// Missing optional metadata cannot satisfy the explicit customer constraint.
		if candidate == "" {
			continue
		}
		candidateURL, err := canonicalDiscoveryMatchURL(candidate, allowedHosts)
		// Provider-returned match values obey the same host boundary as customer-derived URLs.
		if err != nil {
			return Resource{}, 0, errors.New("discovery match metadata is invalid")
		}
		// Equality is evaluated only after safe structural canonicalization.
		if candidateURL == expectedURL {
			matched = resource
			matchCount++
		}
	}
	return matched, matchCount, nil
}

// discoveryMatchMetadataValue reads one declared scalar without exposing the
// remaining provider metadata to matching or error output.
func discoveryMatchMetadataValue(raw []byte, key string) (string, error) {
	values := map[string]string{}
	// Only scalar strings are valid URL match values; structured metadata fails closed.
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", errors.New("discovery match metadata is invalid")
	}
	return strings.TrimSpace(values[key]), nil
}

// canonicalDiscoveryMatchURL normalizes only safe structural URL differences;
// path case remains significant and query/fragment values are rejected.
func canonicalDiscoveryMatchURL(raw string, allowedHosts []string) (string, error) {
	// Match URLs inherit the reviewed resource-input host boundary.
	if err := ValidateBaseURL(raw, allowedHosts); err != nil {
		return "", err
	}
	parsed, _ := url.Parse(raw)
	// Query and fragment data must not influence tenant identity.
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("discovery match URL cannot contain query or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

// NormalizeInput validates every supplied declared field while reporting
// required omissions separately. Connect callers can therefore launch a
// collection page only for absent data, without turning invalid submitted data
// into an interactive fallback or persisting undeclared values.
func NormalizeInput(config *fusedobject.ResourceInputConfig, values map[string]string) (map[string]string, []string, error) {
	normalized := make(map[string]string, len(config.Fields))
	missing := make([]string, 0, len(config.Fields))
	declared := make(map[string]struct{}, len(config.Fields))
	for _, field := range config.Fields {
		declared[field.Name] = struct{}{}
		value := strings.TrimSpace(values[field.Name])
		if field.Required && value == "" {
			missing = append(missing, field.Name)
		}
		if field.Pattern != "" && value != "" {
			matched, err := regexp.MatchString(field.Pattern, value)
			if err != nil || !matched {
				return nil, nil, fmt.Errorf("resource input %q is invalid", field.Name)
			}
		}
		normalized[field.Name] = value
	}
	// Undeclared keys fail before persistence so caller mistakes cannot become
	// unreviewed customer metadata or collide with future routing controls.
	for name := range values {
		if _, ok := declared[name]; !ok {
			return nil, nil, fmt.Errorf("resource input %q is not declared", name)
		}
	}
	return normalized, missing, nil
}

// ValidateBaseURL confines dynamic routing to HTTPS hosts authorized by the
// versioned service declaration; localhost HTTP remains available for dev.
func ValidateBaseURL(raw string, allowedHosts []string) error {
	parsed, err := url.Parse(raw)
	// User-info and hostless URLs blur the dispatch authority and are rejected.
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("connection resource URL is invalid")
	}
	// Plain HTTP is reserved for loopback development, never provider hosts.
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLocalHost(parsed.Hostname())) {
		return errors.New("connection resource URL must use https")
	}
	// Dynamic routing requires an explicit versioned allowlist.
	if len(allowedHosts) == 0 {
		return errors.New("connection resource host allowlist is required")
	}
	// Parsed host comparison resists raw-authority suffix and user-info tricks.
	if !hostAllowed(strings.ToLower(parsed.Hostname()), allowedHosts) {
		return errors.New("connection resource host is not allowed")
	}
	return nil
}

// resourceAt extracts one aligned resource and validates its final rendered
// URL before the Engine can persist it as trusted dispatch metadata.
func resourceAt(payload any, config *fusedobject.ResourceDiscoveryConfig, metadataPaths map[string]string, index int, id string) (Resource, error) {
	name := indexedString(payload, config.NamePath, index)
	// Provider identity is the fallback when no display name was declared.
	if name == "" {
		name = id
	}
	values := map[string]string{"id": id, "name": name}
	baseURL, err := discoveredBaseURL(payload, config, values, index)
	if err != nil {
		return Resource{}, err
	}
	metadataValues, err := extractedMetadata(payload, metadataPaths, index)
	if err != nil {
		return Resource{}, err
	}
	metadata, _ := json.Marshal(metadataValues)
	return Resource{
		ProviderID: id,
		Type:       config.ResourceType,
		Name:       name,
		BaseURL:    baseURL,
		Metadata:   metadata,
		Scopes:     indexedStrings(payload, config.ScopesPath, index),
	}, nil
}

// discoveredBaseURL leaves routing empty for static-host services whose
// connected resource is used only by header, path, or query bindings.
func discoveredBaseURL(payload any, config *fusedobject.ResourceDiscoveryConfig, values map[string]string, index int) (string, error) {
	// Context-only resources intentionally retain the static Registry base URL.
	if config.BaseURLPath == "" && config.BaseURLTemplate == "" {
		return "", nil
	}
	baseURL := indexedString(payload, config.BaseURLPath, index)
	// A response path wins; templates cover providers returning only IDs.
	if baseURL == "" {
		baseURL = renderTemplate(config.BaseURLTemplate, values)
	}
	if err := ValidateBaseURL(baseURL, config.AllowedHosts); err != nil {
		return "", err
	}
	return baseURL, nil
}

// extractedMetadata sorts declared fields for deterministic stored JSON and
// ignores absent optional values without shifting another resource's context.
func extractedMetadata(payload any, paths map[string]string, index int) (map[string]any, error) {
	metadata := make(map[string]any, len(paths))
	keys := make([]string, 0, len(paths))
	// Sorting keys makes persisted metadata and audit behavior deterministic.
	for key := range paths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values, err := pathValues(payload, paths[key])
		// Optional missing metadata is omitted without shifting resource rows.
		if err != nil || index >= len(values) || values[index] == nil {
			continue
		}
		// Structured provider objects are not safe transport binding values.
		if !isScalarMetadata(values[index]) {
			return nil, errors.New("resource discovery metadata must be scalar")
		}
		metadata[key] = values[index]
	}
	return metadata, nil
}

// isScalarMetadata limits bindings to values with an unambiguous header,
// query, or path representation.
func isScalarMetadata(value any) bool {
	switch value.(type) {
	case string, json.Number, bool:
		return true
	default:
		return false
	}
}

// discoveryBaseURL selects only a Registry-owned named server; unknown names
// fail later URL validation instead of falling back to another environment.
func discoveryBaseURL(metadata *fusedobject.ServiceMetadata, serverName string) string {
	// An empty name means the version's selected/default server.
	if serverName == "" {
		return metadata.BaseURL
	}
	// Named servers match only imported version metadata.
	for _, server := range metadata.Servers {
		if server.Environment == serverName {
			return server.URL
		}
	}
	return ""
}

// pathValues implements the intentionally small `$[*].field`/`$.field` subset
// documented for x-fused-connect, keeping configuration portable and bounded.
func pathValues(payload any, path string) ([]any, error) {
	path = strings.TrimSpace(path)
	// Empty optional paths return no values, not the whole document.
	if path == "" {
		return nil, nil
	}
	// Array expansion is root-only so index alignment remains predictable.
	if strings.HasPrefix(path, "$[*].") {
		items, ok := payload.([]any)
		if !ok {
			return nil, errors.New("resource discovery array path requires an array response")
		}
		field := strings.TrimPrefix(path, "$[*].")
		values := make([]any, 0, len(items))
		for _, item := range items {
			values = append(values, nestedValue(item, field))
		}
		return values, nil
	}
	// Object paths return one value because there is no array alignment dimension.
	if strings.HasPrefix(path, "$.") {
		return []any{nestedValue(payload, strings.TrimPrefix(path, "$."))}, nil
	}
	return nil, errors.New("resource discovery path is unsupported")
}

// nestedValue walks object fields only; array expansion is deliberately owned
// by pathValues so extraction cannot grow an unbounded recursive query engine.
func nestedValue(value any, path string) any {
	current := value
	// Missing or non-object segments terminate lookup instead of coercing data.
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[segment]
	}
	return current
}

// indexedString safely reads a parallel JSONPath result without permitting a
// short optional field list to shift later resources onto the wrong tenant.
func indexedString(payload any, path string, index int) string {
	values, err := pathValues(payload, path)
	// Optional text fields collapse missing, invalid, or short results to empty.
	if err != nil || index >= len(values) || values[index] == nil {
		return ""
	}
	value, _ := discoveryScalarString(values[index])
	return value
}

// indexedStrings normalizes either an array of scope names or a scalar scope
// string while preserving alignment with the selected resource.
func indexedStrings(payload any, path string, index int) []string {
	values, err := pathValues(payload, path)
	// Scope extraction is optional and must never shift onto another resource.
	if err != nil || index >= len(values) {
		return nil
	}
	// Providers commonly return either a JSON array or a space-delimited string.
	switch value := values[index].(type) {
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if scalar, ok := discoveryScalarString(item); ok {
				out = append(out, scalar)
			}
		}
		return out
	case string:
		return strings.Fields(value)
	default:
		return nil
	}
}

// renderTemplate substitutes only declared scalar values; callers validate the
// resulting URL before trusting it for provider dispatch.
func renderTemplate(template string, values map[string]string) string {
	// Callers provide only validated declared keys, so replacement stays bounded.
	for key, value := range values {
		template = strings.ReplaceAll(template, "{"+key+"}", value)
	}
	return template
}

// discoveryHTTPClient blocks cross-host redirects because Go would otherwise
// forward the bearer token to a provider-selected host.
func discoveryHTTPClient(origin *url.URL) *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Match Go's default redirect ceiling while returning a provider-neutral error.
			if len(via) >= 10 {
				return errors.New("resource discovery redirect limit exceeded")
			}
			// Preserving host and scheme prevents bearer tokens from crossing an
			// authority boundary or being downgraded to plaintext transport.
			if req.URL.User != nil || !strings.EqualFold(req.URL.Host, origin.Host) || !strings.EqualFold(req.URL.Scheme, origin.Scheme) {
				return errors.New("resource discovery cross-host redirect blocked")
			}
			return nil
		},
	}
}

// resolveOperationURL preserves the configured base path while resolving a
// Registry-owned endpoint path; user input never reaches this operation URL.
func resolveOperationURL(baseURL, operationPath string) (string, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/") + "/")
	// Discovery endpoints inherit the same transport boundary as resource URLs.
	if err != nil || base.Hostname() == "" || base.User != nil || (base.Scheme != "https" && !(base.Scheme == "http" && isLocalHost(base.Hostname()))) {
		return "", errors.New("invalid base URL")
	}
	reference, err := url.Parse(strings.TrimLeft(operationPath, "/"))
	if err != nil {
		return "", err
	}
	return base.ResolveReference(reference).String(), nil
}

// defaultTokenType avoids malformed authorization headers when a provider
// omits token_type from an otherwise successful OAuth response.
func defaultTokenType(tokenType string) string {
	// OAuth defines Bearer as the interoperable fallback when providers omit token_type.
	if strings.TrimSpace(tokenType) == "" {
		return "Bearer"
	}
	return tokenType
}

// firstInputDisplayName gives admin surfaces a useful label without persisting
// the entire input object as display text.
func firstInputDisplayName(fields []fusedobject.ResourceInputField, values map[string]string) string {
	// Declaration order expresses which safe customer field is most useful as a label.
	for _, field := range fields {
		if value := values[field.Name]; value != "" {
			return value
		}
	}
	return "Connected resource"
}

// hostAllowed accepts exact hosts and explicit wildcard subdomains while
// rejecting suffix tricks such as allowed.example.attacker.test.
func hostAllowed(host string, allowed []string) bool {
	// Every candidate is normalized before exact or explicit wildcard matching.
	for _, candidate := range allowed {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if host == candidate || (strings.HasPrefix(candidate, "*.") && strings.HasSuffix(host, strings.TrimPrefix(candidate, "*"))) {
			return true
		}
	}
	return false
}

// isLocalHost is the sole HTTP exception and is narrow enough that production
// tenant input cannot route to arbitrary private network names.
func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
