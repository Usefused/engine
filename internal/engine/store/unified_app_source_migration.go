package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const unifiedSourceMigrationBatchSize = 100
const unifiedSourceMigrationMaxBytes = 256 * 1024

var unifiedSourceConfigDeclaration = regexp.MustCompile(`(?m)^[ \t]*export[ \t]+const[ \t]+fusedConfig[ \t]*=`)

// UnifiedAppSourceMigrationResult exposes only aggregate startup progress, never private app source.
type UnifiedAppSourceMigrationResult struct {
	MigratedRows int
	AlreadyOwned int
	SkippedRows  int
}

// UnifiedAppSourceMigrationStore is the startup capability for upgrading saved Unified App authoring.
type UnifiedAppSourceMigrationStore interface {
	MigrateUnifiedAppSources(context.Context) (UnifiedAppSourceMigrationResult, error)
}

type unifiedSourceMigrationRow struct {
	id      uuid.UUID
	desired []byte
}

// MigrateUnifiedAppSources embeds legacy config beside code without changing an applied version or bundle.
func (s *postgresStore) MigrateUnifiedAppSources(ctx context.Context) (UnifiedAppSourceMigrationResult, error) {
	result := UnifiedAppSourceMigrationResult{}
	cursor := uuid.Nil
	// Each page commits independently so startup never holds locks across the full app history.
	for {
		batch, err := s.migrateUnifiedAppSourceBatch(ctx, cursor, &result)
		// A failed page stops startup before private source can be served inconsistently.
		if err != nil {
			return result, err
		}
		// The UUID cursor bounds each transaction and prevents a malformed row from trapping startup.
		if len(batch) == 0 {
			return result, nil
		}
		cursor = batch[len(batch)-1].id
	}
}

// migrateUnifiedAppSourceBatch locks only one bounded page while replicas may start concurrently.
func (s *postgresStore) migrateUnifiedAppSourceBatch(ctx context.Context, after uuid.UUID, result *UnifiedAppSourceMigrationResult) ([]unifiedSourceMigrationRow, error) {
	tx, err := s.db.Begin(ctx)
	// A transaction makes source replacement and its marker atomic.
	if err != nil {
		return nil, fmt.Errorf("begin Unified App source migration: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT id, desired_state FROM fused_config_states
		WHERE config_type = 'unified_app' AND unified_source_migrated_at IS NULL AND id > $1
		ORDER BY id LIMIT $2 FOR UPDATE
	`, after, unifiedSourceMigrationBatchSize)
	// Read errors must leave this page unmarked for a future startup attempt.
	if err != nil {
		return nil, fmt.Errorf("read Unified App migration batch: %w", err)
	}
	batch := make([]unifiedSourceMigrationRow, 0, unifiedSourceMigrationBatchSize)
	// Materialize the locked page before updating any of its rows.
	for rows.Next() {
		var row unifiedSourceMigrationRow
		// A partial page cannot safely commit a subset of editor conversions.
		if err := rows.Scan(&row.id, &row.desired); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan Unified App migration batch: %w", err)
		}
		batch = append(batch, row)
	}
	readErr := rows.Err()
	rows.Close()
	// Iterator failure is equivalent to an incomplete locked page.
	if readErr != nil {
		return nil, fmt.Errorf("read Unified App migration batch: %w", readErr)
	}
	page := UnifiedAppSourceMigrationResult{}
	// Each saved version owns one independent migration decision.
	for _, row := range batch {
		source, status := migratedUnifiedAppSource(row.desired)
		// An unavailable or oversized source remains untouched so the existing runtime stays usable.
		if status == unifiedSourceSkip {
			page.SkippedRows++
			continue
		}
		// An already source-owned app only needs its per-row progress marker.
		if status == unifiedSourceAlreadyOwned {
			// Persist ownership once so later starts do not rescan a valid one-file source.
			if _, err := tx.Exec(ctx, `UPDATE fused_config_states SET unified_source_migrated_at=NOW() WHERE id=$1`, row.id); err != nil {
				return nil, fmt.Errorf("mark Unified App source migration: %w", err)
			}
			page.AlreadyOwned++
			continue
		}
		// The editor source is the only field that changes for a legacy app.
		if _, err := tx.Exec(ctx, `
			UPDATE fused_config_states
			SET desired_state=jsonb_set(desired_state, '{source}', to_jsonb($2::text)), unified_source_migrated_at=NOW()
			WHERE id=$1
		`, row.id, source); err != nil {
			return nil, fmt.Errorf("write Unified App source migration: %w", err)
		}
		page.MigratedRows++
	}
	// A commit failure must not report unpersisted conversions as successful.
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit Unified App source migration: %w", err)
	}
	// Only a committed batch contributes to the startup summary.
	result.MigratedRows += page.MigratedRows
	result.AlreadyOwned += page.AlreadyOwned
	result.SkippedRows += page.SkippedRows
	return batch, nil
}

type unifiedSourceMigrationStatus uint8

const (
	unifiedSourceSkip unifiedSourceMigrationStatus = iota
	unifiedSourceAlreadyOwned
	unifiedSourceMigrated
)

// migratedUnifiedAppSource builds a portable declaration from the exact saved config and code.
func migratedUnifiedAppSource(desired []byte) (string, unifiedSourceMigrationStatus) {
	var config map[string]json.RawMessage
	// Malformed saved state cannot be guessed into executable authoring.
	if err := json.Unmarshal(desired, &config); err != nil || config == nil {
		return "", unifiedSourceSkip
	}
	var kind string
	// A mismatched saved discriminator cannot become a valid Unified App declaration.
	if err := json.Unmarshal(config["kind"], &kind); err != nil || kind != "unified_app" {
		return "", unifiedSourceSkip
	}
	var source string
	// Bundle-only versions lack editable TypeScript and retain their old private record.
	if err := json.Unmarshal(config["source"], &source); err != nil || strings.TrimSpace(source) == "" {
		return "", unifiedSourceSkip
	}
	// A valid exported literal means this app already uses the one-file authoring form.
	if hasUnifiedSourceConfig(source) {
		return "", unifiedSourceAlreadyOwned
	}
	delete(config, "source")
	delete(config, "source_path")
	delete(config, "bundle_digest")
	// Type was a local YAML file discriminator and is invalid in source-owned metadata.
	delete(config, "type")
	metadata, err := json.MarshalIndent(config, "", "  ")
	// Unknown malformed saved metadata cannot be guessed into portable TypeScript.
	if err != nil {
		return "", unifiedSourceSkip
	}
	combined := "export const fusedConfig = " + string(metadata) + ";\n\n" + source
	// The same source ceiling used by CLI plans prevents exporting an unusable replacement.
	if len(combined) > unifiedSourceMigrationMaxBytes {
		return "", unifiedSourceSkip
	}
	return combined, unifiedSourceMigrated
}

// hasUnifiedSourceConfig recognizes a literal exported declaration before startup adds another one.
func hasUnifiedSourceConfig(source string) bool {
	candidates := unifiedSourceConfigDeclaration.FindAllStringIndex(source, -1)
	const (
		code = iota
		lineComment
		blockComment
		singleQuote
		doubleQuote
		templateQuote
	)
	state := code
	nextCandidate := 0
	// Scan lexical context so a comment example never suppresses a real migration.
	for index := 0; index < len(source); index++ {
		// Examples inside comments or strings do not establish source ownership.
		if nextCandidate < len(candidates) && index == candidates[nextCandidate][0] {
			// Only a candidate in live code can establish source ownership.
			if state == code {
				decoder := json.NewDecoder(strings.NewReader(source[candidates[nextCandidate][1]:]))
				var config map[string]json.RawMessage
				// Only a literal object can satisfy the CLI's declaration contract.
				if err := decoder.Decode(&config); err == nil && config != nil {
					return true
				}
			}
			nextCandidate++
		}
		character := source[index]
		// Keep the lexical states aligned with the CLI's declaration discovery.
		switch state {
		case code:
			// Comment openers hide declaration-shaped examples until their terminator.
			if character == '/' && index+1 < len(source) && source[index+1] == '/' {
				state, index = lineComment, index+1
			} else if character == '/' && index+1 < len(source) && source[index+1] == '*' {
				state, index = blockComment, index+1
			} else if character == '\'' {
				state = singleQuote
			} else if character == '"' {
				state = doubleQuote
			} else if character == '`' {
				state = templateQuote
			}
		case lineComment:
			// A newline restores normal code scanning for the next candidate.
			if character == '\n' {
				state = code
			}
		case blockComment:
			// A block close restores code even when the example spans many lines.
			if character == '*' && index+1 < len(source) && source[index+1] == '/' {
				state, index = code, index+1
			}
		case singleQuote, doubleQuote, templateQuote:
			// Escaped delimiters cannot end a quoted example early.
			if character == '\\' {
				index++
			} else if (state == singleQuote && character == '\'') || (state == doubleQuote && character == '"') || (state == templateQuote && character == '`') {
				state = code
			}
		}
	}
	return false
}
