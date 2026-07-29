package connectionprofile

import (
	"errors"
	"regexp"
	"strings"

	"github.com/Usefused/engine/internal/shared/secretref"
)

var (
	environmentExpression = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	metadataSourcePath    = regexp.MustCompile(`^metadata\.([A-Za-z_][A-Za-z0-9_.-]{0,127})$`)
)

var errInvalidExpression = errors.New("binding value expression is invalid")

// ParseExpression classifies the complete value grammar. Errors never include
// the supplied value because it may be a local environment-derived secret.
func ParseExpression(value string) (Expression, error) {
	// Whole-value environment references are resolved only by workspace apply.
	if match := environmentExpression.FindStringSubmatch(value); match != nil {
		return Expression{Kind: SourceEnvironment, Raw: value, EnvName: match[1]}, nil
	}
	// Template-looking input must satisfy the closed resource grammar; treating
	// malformed interpolation as a literal would hide a configuration mistake.
	if strings.HasPrefix(value, "${") || strings.Contains(value, "${resource.") {
		return parseDynamicExpression(value)
	}
	// A remaining dollar prefix is an invalid environment spelling, not a secret literal.
	if strings.HasPrefix(value, "$") {
		return Expression{}, errInvalidExpression
	}
	return Expression{Kind: SourceLiteral, Raw: value}, nil
}

// parseDynamicExpression accepts only complete resource references so profile
// values cannot become a general interpolation language at dispatch time.
// The whole-value tag check is shared with secretref's bucket references
// (see secretref.SingleTag) since both are standalone reference fields, not
// templates -- only the "resource.*" closed list below is specific to this
// package's connection-resource domain.
func parseDynamicExpression(value string) (Expression, error) {
	inner, ok := secretref.SingleTag(value)
	if !ok || !strings.HasPrefix(inner, "resource.") {
		return Expression{}, errInvalidExpression
	}
	path := strings.TrimPrefix(inner, "resource.")
	// Only stable identity, trusted routing URL, and declared metadata are addressable.
	if path == "provider_resource_id" || path == "base_url" || metadataSourcePath.MatchString(path) {
		return Expression{Kind: SourceConnectionResource, Raw: value, SourcePath: path}, nil
	}
	return Expression{}, errInvalidExpression
}
