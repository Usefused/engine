package store

import (
	"bytes"
	"fmt"
	"regexp"
)

// Retired syntax is understood only by this one-time data conversion, never by an authoring reader.
var legacyUnifiedSourceConfigDeclaration = regexp.MustCompile(`(?m)^[ \t]*export[ \t]+const[ \t]+fusedConfig[ \t]*=`)
var retiredConfigReference = regexp.MustCompile(`\bfusedConfig\b`)
var templateConfigReference = regexp.MustCompile(`\$\{[^}]*\bfusedConfig\b`)

// hasRetiredUnifiedMetadata identifies stored records needing conversion even when they also contain current config.
func hasRetiredUnifiedMetadata(source string) bool {
	return legacyUnifiedSourceConfigDeclaration.Match(inlineConfigCodeMask([]byte(source)))
}

// legacyUnifiedMetadataRange bounds only the retired literal declaration for the one-time conversion.
func legacyUnifiedMetadataRange(source []byte) (int, int, error) {
	matches := legacyUnifiedSourceConfigDeclaration.FindAllIndex(inlineConfigCodeMask(source), -1)
	// Missing metadata is the original YAML-derived source format and needs only inline insertion.
	if len(matches) == 0 {
		return -1, -1, nil
	}
	// Duplicate declarations cannot be safely interpreted as one saved metadata binding.
	if len(matches) != 1 {
		return 0, 0, fmt.Errorf("multiple retired metadata declarations")
	}
	end, err := literalSourceMetadata(source, matches[0][1])
	// Only the historical JSON-literal format can be converted without evaluating app code.
	if err != nil {
		return 0, 0, err
	}
	tail := source[end:]
	trimmed := bytes.TrimLeft(tail, " \t")
	// The original statement's semicolon belongs to metadata, while comments and implementation bytes do not.
	if bytes.HasPrefix(trimmed, []byte(";")) {
		end += len(tail) - len(trimmed) + 1
	}
	return matches[0][0], end, nil
}

// removeLegacyUnifiedMetadata deletes only the retired declaration, preserving every unrelated code and comment byte.
func removeLegacyUnifiedMetadata(source []byte) ([]byte, error) {
	start, end, err := legacyUnifiedMetadataRange(source)
	// Malformed declarations must remain untouched and unmarked for explicit repair.
	if err != nil {
		return nil, err
	}
	// Implementation-only source has no retired declaration to remove.
	if start < 0 {
		return source, nil
	}
	result := append(append([]byte(nil), source[:start]...), source[end:]...)
	// A referenced binding needs an explicit source rewrite rather than silently creating a broken future deployment.
	if retiredConfigReference.Match(inlineConfigCodeMask(result)) || templateConfigReference.Match(result) {
		return nil, fmt.Errorf("retired metadata binding is referenced by executable source")
	}
	return result, nil
}

// inlineUnifiedSourceMetadata converts either stored predecessor format into the one supported inline config format.
func inlineUnifiedSourceMetadata(source string, metadata []byte) (string, error) {
	data, err := removeLegacyUnifiedMetadata([]byte(source))
	// Failed removal must never produce a partially migrated source file.
	if err != nil {
		return "", err
	}
	starts, err := inlineConfigDeclarations(data)
	// Root discovery rejects ambiguous app definitions and options that could override static metadata.
	if err != nil {
		return "", err
	}
	// Multiple config properties cannot be assigned a single portable authority.
	if len(starts) > 1 {
		return "", fmt.Errorf("multiple config properties")
	}
	// Existing inline config beside a retired export is replaced from the authoritative persisted settings.
	if len(starts) == 1 {
		return replaceUnifiedSourceMetadata(data, starts[0], metadata)
	}
	start, err := unifiedOptionsStart(inlineConfigCodeMask(data))
	// A nonliteral default app cannot be rewritten without changing its implementation.
	if err != nil {
		return "", err
	}
	result := append([]byte(nil), data[:start]...)
	result = append(result, []byte("\n  config: ")...)
	result = append(result, bytes.ReplaceAll(metadata, []byte("\n"), []byte("\n  "))...)
	result = append(result, ',')
	result = append(result, data[start:]...)
	return string(result), nil
}

// replaceUnifiedSourceMetadata updates only a literal initializer after removal of its retired competitor.
func replaceUnifiedSourceMetadata(source []byte, start int, metadata []byte) (string, error) {
	end, err := literalSourceMetadata(source, start)
	// A malformed initializer remains untouched so the saved source can be repaired deliberately.
	if err != nil {
		return "", err
	}
	result := append([]byte(nil), source[:start]...)
	result = append(result, metadata...)
	result = append(result, source[end:]...)
	return string(result), nil
}
