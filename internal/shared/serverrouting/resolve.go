package serverrouting

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

var (
	variableNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
	placeholderPattern   = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_.-]*)\}`)
	variableValuePattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

const suppliedHostMarker = "fused-supplied-variable"

// IsVariableName exposes the placeholder grammar already enforced by runtime
// resolution so configuration validation cannot drift from dispatch.
func IsVariableName(value string) bool {
	return variableNamePattern.MatchString(value)
}

// ValidateResolvedHostAnchor confines app-supplied hostname variables to the
// registrable provider domain that remains immutable in the template.
func ValidateResolvedHostAnchor(template, resolved string, supplied map[string]bool) error {
	marked, err := url.Parse(markServerVariables(template, supplied))
	// Variables outside the authority cannot select the request destination.
	if err != nil || !strings.Contains(marked.Hostname(), suppliedHostMarker) {
		return nil
	}
	anchor, err := publicsuffix.EffectiveTLDPlusOne(marked.Hostname())
	// A marker in the eTLD+1 means caller data controls the registrable domain;
	// whole-host and private/public-suffix templates therefore fail closed.
	if err != nil || strings.Contains(anchor, suppliedHostMarker) {
		return errors.New("server variable requires a fixed provider domain")
	}
	parsed, err := url.Parse(resolved)
	// Malformed resolved URLs cannot establish a trustworthy destination host.
	if err != nil {
		return errors.New("resolved server URL is invalid")
	}
	host := strings.ToLower(parsed.Hostname())
	anchor = strings.ToLower(anchor)
	// Exact or subdomain matching prevents escape to a sibling registrable domain.
	if host != anchor && !strings.HasSuffix(host, "."+anchor) {
		return errors.New("server variable escaped the provider host")
	}
	return nil
}

// markServerVariables makes supplied-variable positions visible to URL and
// public-suffix parsing without resolving any user-controlled value.
func markServerVariables(template string, supplied map[string]bool) string {
	return placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		name := placeholderPattern.FindStringSubmatch(match)[1]
		// Only app-owned variables need the stronger registrable-domain boundary.
		if supplied[name] {
			return suppliedHostMarker
		}
		return "fused-provider-variable"
	})
}

func Resolve(template string, variables []Variable, supplied map[string]string) (string, bool, error) {
	resolved, usedSupplied, err := ResolveReference(template, variables, supplied)
	if err != nil {
		return "", false, err
	}
	if err := ValidateResolvedURL(resolved); err != nil {
		return "", false, err
	}
	return resolved, usedSupplied, nil
}

// ResolveReference performs template substitution without requiring an
// absolute URL. Operation-level OpenAPI servers may be relative and are
// validated after they are resolved against the selected service origin.
func ResolveReference(template string, variables []Variable, supplied map[string]string) (string, bool, error) {
	resolved := template
	usedSupplied := false
	definitions := make(map[string]Variable, len(variables))
	for _, variable := range variables {
		definitions[variable.Name] = variable
	}
	for _, match := range placeholderPattern.FindAllStringSubmatch(template, -1) {
		variable, ok := definitions[match[1]]
		if !ok {
			return "", false, errors.New("server template variable definition is missing")
		}
		value, suppliedValue, err := resolveValue(variable, supplied)
		if err != nil {
			return "", false, err
		}
		usedSupplied = usedSupplied || suppliedValue
		resolved = strings.ReplaceAll(resolved, match[0], value)
	}
	if placeholderPattern.MatchString(resolved) || strings.ContainsAny(resolved, "{}\r\n") {
		return "", false, errors.New("server template remains unresolved")
	}
	return resolved, usedSupplied, nil
}

func resolveValue(variable Variable, supplied map[string]string) (string, bool, error) {
	value := strings.TrimSpace(supplied[variable.Name])
	usedSupplied := value != ""
	if value == "" && variable.Default != nil {
		value = strings.TrimSpace(*variable.Default)
	}
	if value == "" {
		return "", false, errors.New("required server variable is unresolved")
	}
	if !variableValuePattern.MatchString(value) {
		return "", false, errors.New("server variable value is invalid")
	}
	if len(variable.Enum) > 0 && !contains(variable.Enum, value) {
		return "", false, errors.New("server variable value is outside its enum")
	}
	return value, usedSupplied, nil
}

func ValidateResolvedURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("resolved server URL is invalid")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return errors.New("resolved server URL must use https")
	}
	return nil
}

func isLoopback(host string) bool {
	host = strings.ToLower(host)
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
