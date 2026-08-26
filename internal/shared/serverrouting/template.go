package serverrouting

import (
	"errors"
	"net/url"
	"strings"
)

// ValidateReferenceTemplate checks authored defaults and URL structure without
// demanding bucket-bound tenant values before an operation is executed.
func ValidateReferenceTemplate(template string, variables []Variable) error {
	values := templateValidationValues(template, variables)
	reference, _, err := ResolveReference(template, variables, values)
	// The ordinary resolver owns declaration, enum, and safe-value validation.
	if err != nil {
		return err
	}
	// Expanded defaults must obey the same bound as authored server URLs.
	if len(reference) > 2048 {
		return errors.New("runtime operation server URL is invalid")
	}
	parsed, err := url.Parse(reference)
	// Relative servers are legal, but credentials and query/fragment routing are not.
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("runtime operation server URL is invalid")
	}
	base := &url.URL{Scheme: "https", Host: "fused.invalid"}
	return ValidateResolvedURL(base.ResolveReference(parsed).String())
}

// templateValidationValues supplies syntax-only representatives for missing
// values; these never mutate the contract or authorize the eventual destination.
func templateValidationValues(template string, variables []Variable) map[string]string {
	values := make(map[string]string)
	for _, variable := range variables {
		// Authored defaults must go through the resolver unchanged so unsafe ones fail.
		if variable.Default != nil {
			continue
		}
		value := "443"
		// A complete scheme placeholder needs a valid scheme, not a numeric host token.
		if strings.HasPrefix(template, "{"+variable.Name+"}://") {
			value = "https"
		}
		// An enum is an authored allowlist, including when its value is supplied later.
		if len(variable.Enum) > 0 && !contains(variable.Enum, value) {
			value = variable.Enum[0]
		}
		values[variable.Name] = value
	}
	return values
}
