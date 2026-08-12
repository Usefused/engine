package serverrouting

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var (
	placeholderPattern   = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_.-]*)\}`)
	variableValuePattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

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
