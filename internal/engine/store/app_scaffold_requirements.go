package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

const MaxAppScaffoldSelections = 100

var ErrAppScaffoldSelectionUnavailable = errors.New("app scaffold selection is unavailable")

// AppScaffoldSelectionRef keeps authoring keys separate from resolved runtime
// identity so callers cannot mistake a mutable label for execution authority.
type AppScaffoldSelectionRef struct {
	SelectionIndex int
	ServiceKey     string
	Version        string
}

// AppScaffoldResolvedSelection is the exact active workspace version used for
// bounded contract-snapshot reads after the service.read scope is applied.
type AppScaffoldResolvedSelection struct {
	SelectionIndex   int
	ServiceKey       string
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
}

// AppScaffoldSelectionStore resolves a complete authoring batch in one SQL
// statement rather than performing a workspace lookup for every service.
type AppScaffoldSelectionStore interface {
	ResolveAuthorizedAppScaffoldSelections(ctx context.Context, scope accesscontrol.AuthorizedScope, refs []AppScaffoldSelectionRef) ([]AppScaffoldResolvedSelection, error)
}

const resolveAuthorizedAppScaffoldSelectionsSQL = `
	WITH requested AS (
		SELECT *
		FROM unnest($1::integer[], $2::text[], $3::text[])
			AS input(selection_index, service_key, version)
	), matched AS (
		SELECT requested.selection_index, requested.service_key,
		       COUNT(candidate.service_id) AS match_count,
		       CASE WHEN COUNT(candidate.service_id) = 1
		            THEN (array_agg(candidate.service_id ORDER BY candidate.service_id))[1]
		       END AS service_id,
		       CASE WHEN COUNT(candidate.service_version_id) = 1
		            THEN (array_agg(candidate.service_version_id ORDER BY candidate.service_id))[1]
		       END AS service_version_id
		FROM requested
		LEFT JOIN LATERAL (
			SELECT services.service_id, versions.service_version_id
			FROM fused_workspace_services services
			JOIN fused_workspace_service_versions versions
			  ON versions.service_id = services.service_id
			 AND versions.version = requested.version
			 AND versions.status <> 'deprecated'
			WHERE ($4 OR services.service_id = ANY($5::uuid[]))
			  AND (
				COALESCE(services.service_slug, '') = requested.service_key
				OR COALESCE(services.service_name, '') = requested.service_key
				OR (
					requested.service_key LIKE '@%/%'
					AND COALESCE(services.service_slug, '') = split_part(requested.service_key, '/', 2)
				)
			  )
		) candidate ON TRUE
		GROUP BY requested.selection_index, requested.service_key
	)
	SELECT selection_index, service_key, match_count, service_id, service_version_id
	FROM matched
	ORDER BY selection_index`

// ResolveAuthorizedAppScaffoldSelections binds every requested key/version to
// one authorized active row and rejects absent or ambiguous labels uniformly.
func (s *postgresStore) ResolveAuthorizedAppScaffoldSelections(ctx context.Context, scope accesscontrol.AuthorizedScope, refs []AppScaffoldSelectionRef) ([]AppScaffoldResolvedSelection, error) {
	// Empty callers retain a non-nil result without issuing a database query.
	if len(refs) == 0 {
		return []AppScaffoldResolvedSelection{}, nil
	}
	// An empty authorized scope cannot resolve any workspace service; avoiding
	// the query also makes denied collection reads constant at this boundary.
	if !scope.All && len(scope.IDs) == 0 {
		return nil, ErrAppScaffoldSelectionUnavailable
	}
	// The persistence boundary repeats the API cap so another adapter cannot
	// turn the parallel-array query into an unbounded read later.
	if len(refs) > MaxAppScaffoldSelections {
		return nil, ErrAppScaffoldSelectionUnavailable
	}
	indexes, keys, versions := appScaffoldSelectionArrays(refs)
	rows, err := s.db.Query(ctx, resolveAuthorizedAppScaffoldSelectionsSQL, indexes, keys, versions, scope.All, scope.IDs)
	// Query errors remain operational failures rather than looking like an
	// unauthorized or missing service selection.
	if err != nil {
		return nil, fmt.Errorf("resolve app scaffold selections: query: %w", err)
	}
	defer rows.Close()
	return collectAppScaffoldResolvedSelections(rows, len(refs))
}

type appScaffoldSelectionRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

// collectAppScaffoldResolvedSelections enforces exact one-row and one-match
// cardinality independently of PostgreSQL transport details.
func collectAppScaffoldResolvedSelections(rows appScaffoldSelectionRows, expected int) ([]AppScaffoldResolvedSelection, error) {
	resolved := make([]AppScaffoldResolvedSelection, 0, expected)
	// PostgreSQL has already filtered and counted candidates; Go only decodes
	// the bounded one-row-per-input result and enforces exact cardinality.
	for rows.Next() {
		var item AppScaffoldResolvedSelection
		var matchCount int
		var serviceID, serviceVersionID *uuid.UUID
		// Nullable IDs distinguish a zero-match row retained by the lateral join.
		if err := rows.Scan(&item.SelectionIndex, &item.ServiceKey, &matchCount, &serviceID, &serviceVersionID); err != nil {
			return nil, fmt.Errorf("resolve app scaffold selections: scan: %w", err)
		}
		// One generic error prevents authorization misses and ambiguous display
		// names from exposing different workspace membership signals.
		if matchCount != 1 || serviceID == nil || serviceVersionID == nil {
			return nil, fmt.Errorf("%w at selection %d", ErrAppScaffoldSelectionUnavailable, item.SelectionIndex)
		}
		item.ServiceID, item.ServiceVersionID = *serviceID, *serviceVersionID
		resolved = append(resolved, item)
	}
	// A row-stream error invalidates the complete batch so callers never use a
	// partial set of immutable service identities.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve app scaffold selections: rows: %w", err)
	}
	// Parallel unnest must produce exactly one output row for every input.
	if len(resolved) != expected {
		return nil, ErrAppScaffoldSelectionUnavailable
	}
	return resolved, nil
}

// appScaffoldSelectionArrays creates aligned typed arrays so PostgreSQL owns
// filtering while the caller's deterministic selection index survives joins.
func appScaffoldSelectionArrays(refs []AppScaffoldSelectionRef) ([]int32, []string, []string) {
	indexes := make([]int32, len(refs))
	keys := make([]string, len(refs))
	versions := make([]string, len(refs))
	// Positional assignment keeps service keys and versions from drifting
	// across the parallel unnest boundary.
	for index, ref := range refs {
		indexes[index] = int32(ref.SelectionIndex)
		keys[index], versions[index] = ref.ServiceKey, ref.Version
	}
	return indexes, keys, versions
}
