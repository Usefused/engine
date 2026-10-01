package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

var yamlEnvironmentReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandYAMLEnvironment substitutes only scalar values so environment text cannot alter YAML structure.
func expandYAMLEnvironment(node *yaml.Node) error {
	// Alias targets are expanded through their original anchored node, never recursively through aliases.
	if node.Kind == yaml.ScalarNode {
		original := node.Value
		matches := yamlEnvironmentReference.FindAllStringSubmatch(original, -1)
		for _, match := range matches {
			// Missing references fail explicitly; errors include the variable name but never its contents.
			if _, exists := os.LookupEnv(match[1]); !exists {
				return fmt.Errorf("configuration references unset environment variable %s", match[1])
			}
		}
		node.Value = yamlEnvironmentReference.ReplaceAllStringFunc(original, func(reference string) string {
			// Replacement is a single pass: dollar expressions inside credentials remain literal.
			return os.Getenv(reference[2 : len(reference)-1])
		})
		// A whole-scalar reference may supply an integer or boolean to a typed configuration field.
		if len(matches) == 1 && original == matches[0][0] {
			node.Tag = ""
			node.Style = 0
		}
		return nil
	}
	for index, child := range node.Content {
		// Only mapping values are configurable; deployment variables cannot rename configuration keys.
		if node.Kind == yaml.MappingNode && index%2 == 0 {
			continue
		}
		// Abort on the first missing reference without exposing other scalar contents.
		if err := expandYAMLEnvironment(child); err != nil {
			return err
		}
	}
	return nil
}
