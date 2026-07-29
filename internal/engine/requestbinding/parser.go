package requestbinding

import (
	"regexp"
)

var variablePattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// ExtractVariables parses the input string to find all interpolation tags formatted as ${...}.
// It returns a slice of the exact keys inside the tags (e.g., "bucket.env.API_KEY").
func ExtractVariables(input string) []string {
	matches := variablePattern.FindAllStringSubmatch(input, -1)
	if len(matches) == 0 {
		return nil
	}

	var keys []string
	for _, match := range matches {
		if len(match) == 2 {
			keys = append(keys, match[1])
		}
	}
	return keys
}

// Interpolate replaces all interpolation tags in the input string with their corresponding
// values from the provided map. If a key is missing from the map, it leaves the tag unresolved.
func Interpolate(input string, values map[string]string) string {
	return variablePattern.ReplaceAllStringFunc(input, func(match string) string {
		// match is the full tag, e.g., "${bucket.env.API_KEY}"
		// We need to extract the key itself
		key := match[2 : len(match)-1] // Strip prefix "${" and suffix "}"
		if val, ok := values[key]; ok {
			return val
		}
		// If not found in map, leave it as is
		return match
	})
}
