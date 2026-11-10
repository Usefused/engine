package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

var unifiedAppOptions = regexp.MustCompile(`\bexport\s+default\s+buildUnifiedApp\s*\(\s*\{`)
var inlineAppConfig = regexp.MustCompile(`^\s*config\s*:\s*`)

// sourceRegionEnd bounds comments without ever interpreting tenant-authored content.
func sourceRegionEnd(source []byte, start int, suffix []byte) int {
	offset := bytes.Index(source[start:], suffix)
	// An unfinished non-code region remains masked through the end of the file.
	if offset < 0 {
		return len(source)
	}
	return start + offset + len(suffix)
}

// sourceQuotedEnd skips escaped delimiters so quoted examples cannot become structural syntax.
func sourceQuotedEnd(source []byte, start int) int {
	for index := start + 1; index < len(source); index++ {
		// Escaped delimiters remain within this same quoted region.
		if source[index] == '\\' {
			index++
			continue
		}
		// The matching quote ends this region; other quote kinds remain ordinary contents.
		if source[index] == source[start] {
			return index + 1
		}
	}
	return len(source)
}

// sourceNonCodeEnd recognizes only lexical regions that cannot declare app options or exports.
func sourceNonCodeEnd(source []byte, index int) int {
	// Line comments may contain declaration-shaped examples until the next newline.
	if bytes.HasPrefix(source[index:], []byte("//")) {
		return sourceRegionEnd(source, index+2, []byte("\n"))
	}
	// Block examples remain non-code across every embedded newline.
	if bytes.HasPrefix(source[index:], []byte("/*")) {
		return sourceRegionEnd(source, index+2, []byte("*/"))
	}
	// All supported quote kinds hide structural braces from the options scanner.
	if bytes.ContainsRune([]byte("\"'`"), rune(source[index])) {
		return sourceQuotedEnd(source, index)
	}
	return index
}

// inlineConfigCodeMask preserves offsets and line boundaries while excluding comments and quoted examples.
func inlineConfigCodeMask(source []byte) []byte {
	masked := append([]byte(nil), source...)
	for index := 0; index < len(source); {
		end := sourceNonCodeEnd(source, index)
		// Live code retains its bytes for exact identifier and delimiter matching.
		if end == index {
			index++
			continue
		}
		for index < end {
			// Line boundaries keep anchored declaration searches aligned with the original file.
			if source[index] != '\n' && source[index] != '\r' {
				masked[index] = ' '
			}
			index++
		}
	}
	return masked
}

// unifiedOptionsStart requires one default literal so migration never inserts metadata into an unrelated call.
func unifiedOptionsStart(masked []byte) (int, error) {
	apps := unifiedAppOptions.FindAllIndex(masked, -1)
	// A missing or ambiguous default app cannot be rewritten without changing authored implementation.
	if len(apps) != 1 {
		return 0, fmt.Errorf("use one default buildUnifiedApp({ ... }) declaration")
	}
	return apps[0][1], nil
}

// directInlineConfig finds one root property and rejects spreads that could override reviewed metadata.
func directInlineConfig(masked []byte, index int) (int, error) {
	// A spread can supersede the source-owned deployment configuration.
	if bytes.HasPrefix(bytes.TrimSpace(masked[index:]), []byte("...")) {
		return -1, fmt.Errorf("use explicit buildUnifiedApp properties instead of an options spread")
	}
	match := inlineAppConfig.FindIndex(masked[index:])
	// Other options belong to executable code and remain unchanged.
	if match == nil {
		return -1, nil
	}
	return index + match[1], nil
}

var sourceDelimiterDepth = map[byte]int{'{': 1, '[': 1, '(': 1, '}': -1, ']': -1, ')': -1}

// inlinePropertyStarts visits direct options while leaving nested schemas and method bodies out of metadata discovery.
func inlinePropertyStarts(masked []byte, start int) ([]int, error) {
	starts := make([]int, 0)
	depth, propertyStart := 0, start
	for index := start; index < len(masked); index++ {
		// Only direct properties can provide app-level deployment metadata.
		if depth == 0 && index == propertyStart {
			value, err := directInlineConfig(masked, index)
			// Conflicting options must not be rewritten into an apparently valid app.
			if err != nil {
				return nil, err
			}
			// A named config property contributes exactly one candidate.
			if value >= 0 {
				starts = append(starts, value)
			}
		}
		character := masked[index]
		// Stop before any statements following the default app's options.
		if depth == 0 && character == '}' {
			break
		}
		depth += sourceDelimiterDepth[character]
		// Root commas begin another option rather than a nested schema field.
		if depth == 0 && character == ',' {
			propertyStart = index + 1
		}
	}
	return starts, nil
}

// inlineConfigDeclarations admits only the current config property, never the retired metadata export.
func inlineConfigDeclarations(source []byte) ([]int, error) {
	masked := inlineConfigCodeMask(source)
	start, err := unifiedOptionsStart(masked)
	// Only a uniquely located literal options object can own source metadata.
	if err != nil {
		return nil, err
	}
	return inlinePropertyStarts(masked, start)
}

// literalSourceMetadata bounds one static JSON object without executing any authored expression.
func literalSourceMetadata(source []byte, start int) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(source[start:]))
	var metadata map[string]json.RawMessage
	// Null, arrays and computed expressions cannot establish portable deployment metadata.
	if err := decoder.Decode(&metadata); err != nil || metadata == nil {
		return 0, fmt.Errorf("config must be a literal JSON object")
	}
	return start + int(decoder.InputOffset()), nil
}

// hasInlineUnifiedConfig recognizes only canonical source so startup can mark it without rewriting any bytes.
func hasInlineUnifiedConfig(source string) bool {
	data := []byte(source)
	starts, err := inlineConfigDeclarations(data)
	// Missing or duplicate properties must not be mistaken for a completed format migration.
	if err != nil || len(starts) != 1 {
		return false
	}
	_, err = literalSourceMetadata(data, starts[0])
	return err == nil
}
