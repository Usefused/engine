// Package migration provides the app-family migration tooling for Phase 0/1
// of the artifact-to-app migration. It is read-only during dry-run and produces
// a machine-readable report of proposed groupings, conflicts, and split-mapping
// requirements. No mutation occurs until the backfill phase.
package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/shared/canonical"
)

// FamilyProposal represents one proposed app family with its member apps.
type FamilyProposal struct {
	AppFamilyID    uuid.UUID       `json:"-"`
	AccountID      uuid.UUID       `json:"account_id"`
	Kind           string          `json:"kind"`
	CanonicalName  string          `json:"canonical_name"`
	DisplayName    string          `json:"display_name"`
	TargetLanguage string          `json:"target_language,omitempty"`
	Members        []AppMember     `json:"members"`
	OwnerSubjectID *uuid.UUID      `json:"owner_subject_id,omitempty"`
	OwnerTeamID    *uuid.UUID      `json:"owner_team_id,omitempty"`
	BucketID       *uuid.UUID      `json:"bucket_id,omitempty"`
	Tokens         []TokenProposal `json:"tokens"`
	Conflicts      []Conflict      `json:"conflicts,omitempty"`
}

// AppMember is one artifact scope that will become an app version.
type AppMember struct {
	ArtifactID uuid.UUID `json:"artifact_id"`
	AppID      uuid.UUID `json:"app_id"` // preserved from artifact_id
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	ConfigKey  string    `json:"config_key"`
	SourceHash string    `json:"source_hash"`
	Status     string    `json:"status"` // "active" or "deprecated" initially
}

// TokenProposal is a token that will move to the family level.
type TokenProposal struct {
	ID         uuid.UUID `json:"id"`
	TokenHash  string    `json:"-"`
	Name       string    `json:"name"`
	ArtifactID uuid.UUID `json:"artifact_id"`
	LastUsedAt *string   `json:"last_used_at,omitempty"`
	Collision  bool      `json:"collision,omitempty"`
}

// Conflict describes an unresolvable grouping issue.
type Conflict struct {
	Type    string `json:"type"` // "owner_mismatch", "bucket_mismatch", "token_name_collision", "missing_identity"
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// DryRunReport is the full machine-readable migration plan.
type DryRunReport struct {
	TotalArtifactScopes int              `json:"total_artifact_scopes"`
	TotalFamilies       int              `json:"total_families"`
	ConflictFreeGroups  int              `json:"conflict_free_groups"`
	ConflictGroups      int              `json:"conflict_groups"`
	UnresolvedScopes    int              `json:"unresolved_scopes"`
	Families            []FamilyProposal `json:"families"`
}

// rawScope is the minimal row we read from fused_artifact_scopes for grouping.
type rawScope struct {
	AccountID       uuid.UUID
	ArtifactID      uuid.UUID
	Kind            string
	Name            *string
	Version         *string
	ConfigKey       *string
	OwnerSubjectID  *uuid.UUID
	OwnerTeamID     *uuid.UUID
	DeactivatedAt   *string
	SourceHash      *string
	DesiredKind     *string
	DesiredName     *string
	DesiredVersion  *string
	DesiredLanguage *string
}

// rawToken is a row from fused_artifact_tokens.
type rawToken struct {
	ID         uuid.UUID
	ArtifactID uuid.UUID
	TokenHash  string
	Name       string
	LastUsedAt *string
}

// rawBucket is a row from fused_artifact_buckets.
type rawBucket struct {
	ArtifactID uuid.UUID
	BucketID   uuid.UUID
}

// DryRun queries the current artifact state and produces a migration plan
// without mutating anything. It returns a machine-readable report suitable
// for JSON output and CI validation.
func DryRun(ctx context.Context, db *pgxpool.Pool) (*DryRunReport, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.migration.app_family.dry_run")
	defer span.End()

	scopes, err := loadScopes(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("dry-run: load scopes: %w", err)
	}

	tokens, err := loadTokens(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("dry-run: load tokens: %w", err)
	}

	buckets, err := loadBuckets(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("dry-run: load buckets: %w", err)
	}

	span.SetAttributes(
		attribute.Int("scope_count", len(scopes)),
		attribute.Int("token_count", len(tokens)),
		attribute.Int("bucket_count", len(buckets)),
	)

	groups, unresolvedScopes := groupScopesByCanonicalName(scopes)

	report := &DryRunReport{
		TotalArtifactScopes: len(scopes),
		UnresolvedScopes:    len(unresolvedScopes),
	}

	// Build proposals for each group using indexed lookups.
	tokenByArtifact := indexTokensByArtifact(tokens)
	bucketByArtifact := indexBucketsByArtifact(buckets)
	proposals := buildAllProposals(groups, tokenByArtifact, bucketByArtifact)

	sortProposals(proposals)
	countConflicts(proposals, report)

	report.TotalFamilies = len(proposals)
	report.Families = proposals

	span.SetAttributes(
		attribute.Int("total_families", report.TotalFamilies),
		attribute.Int("conflict_free_groups", report.ConflictFreeGroups),
		attribute.Int("conflict_groups", report.ConflictGroups),
		attribute.Int("unresolved_scopes", report.UnresolvedScopes),
	)

	if len(unresolvedScopes) > 0 {
		report.Families = append(report.Families, unresolvedFamily(unresolvedScopes))
	}

	return report, nil
}

// --- grouping ---

type groupKey struct {
	AccountID     uuid.UUID
	Kind          string
	CanonicalName string
}

func groupScopesByCanonicalName(scopes []rawScope) (map[groupKey][]rawScope, []rawScope) {
	groups := make(map[groupKey][]rawScope)
	var unresolved []rawScope
	for _, s := range scopes {
		if !hasValidStructuredIdentity(s) {
			unresolved = append(unresolved, s)
			continue
		}
		canon, _, canonErr := canonical.AppName(*s.Name)
		if canonErr != nil {
			unresolved = append(unresolved, s)
			continue
		}
		key := groupKey{AccountID: s.AccountID, Kind: s.Kind, CanonicalName: canon}
		groups[key] = append(groups[key], s)
	}
	return groups, unresolved
}

func hasValidStructuredIdentity(scope rawScope) bool {
	if missingStructuredIdentity(scope) {
		return false
	}
	if scope.Kind != *scope.DesiredKind || *scope.Name != *scope.DesiredName ||
		*scope.Version != *scope.DesiredVersion {
		return false
	}
	expected := fmt.Sprintf("%s:%s:%s", scope.Kind, *scope.Name, *scope.Version)
	return *scope.ConfigKey == expected
}

func missingStructuredIdentity(scope rawScope) bool {
	return missingScopeIdentity(scope) || missingDesiredIdentity(scope)
}

func missingScopeIdentity(scope rawScope) bool {
	return scope.Name == nil || scope.Version == nil ||
		scope.ConfigKey == nil || scope.SourceHash == nil
}

func missingDesiredIdentity(scope rawScope) bool {
	return scope.DesiredKind == nil || scope.DesiredName == nil ||
		scope.DesiredVersion == nil
}

func indexTokensByArtifact(tokens []rawToken) map[uuid.UUID][]rawToken {
	m := make(map[uuid.UUID][]rawToken)
	for _, t := range tokens {
		m[t.ArtifactID] = append(m[t.ArtifactID], t)
	}
	return m
}

func indexBucketsByArtifact(buckets []rawBucket) map[uuid.UUID]uuid.UUID {
	m := make(map[uuid.UUID]uuid.UUID)
	for _, b := range buckets {
		m[b.ArtifactID] = b.BucketID
	}
	return m
}

func buildAllProposals(
	groups map[groupKey][]rawScope,
	tokenByArtifact map[uuid.UUID][]rawToken,
	bucketByArtifact map[uuid.UUID]uuid.UUID,
) []FamilyProposal {
	var proposals []FamilyProposal
	for key, members := range groups {
		prop := buildProposal(key, members, tokenByArtifact, bucketByArtifact)
		proposals = append(proposals, prop)
	}
	return proposals
}

func sortProposals(proposals []FamilyProposal) {
	sort.Slice(proposals, func(i, j int) bool {
		if proposals[i].AccountID != proposals[j].AccountID {
			return proposals[i].AccountID.String() < proposals[j].AccountID.String()
		}
		if proposals[i].Kind != proposals[j].Kind {
			return proposals[i].Kind < proposals[j].Kind
		}
		return proposals[i].CanonicalName < proposals[j].CanonicalName
	})
}

func countConflicts(proposals []FamilyProposal, report *DryRunReport) {
	for i := range proposals {
		if len(proposals[i].Conflicts) > 0 {
			report.ConflictGroups++
		} else {
			report.ConflictFreeGroups++
		}
	}
}

func unresolvedFamily(scopes []rawScope) FamilyProposal {
	return FamilyProposal{
		CanonicalName: "__unresolved__",
		Conflicts: []Conflict{{
			Type:    "missing_identity",
			Message: fmt.Sprintf("%d scopes have no structured name; cannot group", len(scopes)),
			Details: scopes,
		}},
	}
}

// buildProposal groups members of one (account, kind, name) key into a family
// proposal, detecting owner/bucket/token conflicts.
func buildProposal(
	key groupKey,
	members []rawScope,
	tokenByArtifact map[uuid.UUID][]rawToken,
	bucketByArtifact map[uuid.UUID]uuid.UUID,
) FamilyProposal {
	displayName := resolveDisplayName(key.CanonicalName, members[0].Name)

	prop := FamilyProposal{
		AccountID:     key.AccountID,
		Kind:          key.Kind,
		CanonicalName: key.CanonicalName,
		DisplayName:   displayName,
	}
	prop.TargetLanguage = groupTargetLanguage(members)

	prop.Members = buildMembers(members)
	prop.Tokens, prop.Conflicts = collectTokensAndConflicts(members, tokenByArtifact)
	prop.Conflicts = append(prop.Conflicts, detectOwnerConflicts(members, &prop)...)
	prop.Conflicts = append(prop.Conflicts, detectBucketConflicts(members, bucketByArtifact, &prop)...)
	prop.Conflicts = append(prop.Conflicts, detectLanguageConflicts(key.Kind, members)...)

	sort.Slice(prop.Members, func(i, j int) bool {
		return prop.Members[i].Version < prop.Members[j].Version
	})

	return prop
}

func detectLanguageConflicts(kind string, members []rawScope) []Conflict {
	languages := make(map[string]bool)
	for _, member := range members {
		if member.DesiredLanguage != nil {
			languages[*member.DesiredLanguage] = true
		}
	}
	validSDK := kind == "sdk" && len(languages) == 1 && !languages[""]
	validMCP := kind == "mcp" && (len(languages) == 0 || (len(languages) == 1 && languages[""]))
	if validSDK || validMCP {
		return nil
	}
	return []Conflict{{
		Type:    "language_mismatch",
		Message: "family members have incompatible or missing target languages",
	}}
}

func groupTargetLanguage(members []rawScope) string {
	if len(members) == 0 || members[0].DesiredLanguage == nil {
		return ""
	}
	return *members[0].DesiredLanguage
}

func resolveDisplayName(canonicalName string, rawName *string) string {
	if rawName == nil {
		return canonicalName
	}
	_, disp, err := canonical.AppName(*rawName)
	if err != nil {
		return canonicalName
	}
	return disp
}

func buildMembers(scopes []rawScope) []AppMember {
	members := make([]AppMember, 0, len(scopes))
	for _, m := range scopes {
		am := AppMember{
			ArtifactID: m.ArtifactID,
			AppID:      m.ArtifactID,
			Status:     "active",
		}
		if m.Name != nil {
			am.Name = *m.Name
		}
		if m.Version != nil {
			am.Version = *m.Version
		}
		if m.ConfigKey != nil {
			am.ConfigKey = *m.ConfigKey
		}
		if m.SourceHash != nil {
			am.SourceHash = *m.SourceHash
		}
		if m.DeactivatedAt != nil {
			am.Status = "deactivated"
		}
		members = append(members, am)
	}
	return members
}

func collectTokensAndConflicts(
	members []rawScope,
	tokenByArtifact map[uuid.UUID][]rawToken,
) ([]TokenProposal, []Conflict) {
	var tokens []TokenProposal
	var conflicts []Conflict
	seenNames := make(map[string]bool)

	for _, m := range members {
		for _, tok := range tokenByArtifact[m.ArtifactID] {
			tp := TokenProposal{
				ID:         tok.ID,
				TokenHash:  tok.TokenHash,
				Name:       tok.Name,
				ArtifactID: tok.ArtifactID,
			}
			if tok.LastUsedAt != nil {
				tp.LastUsedAt = tok.LastUsedAt
			}
			if seenNames[tok.Name] {
				tp.Collision = true
				conflicts = append(conflicts, Conflict{
					Type:    "token_name_collision",
					Message: fmt.Sprintf("token name %q is used by multiple artifacts in this family; rename required", tok.Name),
					Details: tok.Name,
				})
			}
			seenNames[tok.Name] = true
			tokens = append(tokens, tp)
		}
	}
	return tokens, conflicts
}

func detectOwnerConflicts(members []rawScope, prop *FamilyProposal) []Conflict {
	ownerSubjects := make(map[uuid.UUID]bool)
	ownerTeams := make(map[uuid.UUID]bool)

	for _, m := range members {
		if m.OwnerSubjectID != nil {
			ownerSubjects[*m.OwnerSubjectID] = true
		}
		if m.OwnerTeamID != nil {
			ownerTeams[*m.OwnerTeamID] = true
		}
	}

	// Conflict: more than one distinct owner or mixed subject/team ownership.
	if len(ownerSubjects) > 1 || len(ownerTeams) > 1 || (len(ownerSubjects) > 0 && len(ownerTeams) > 0) {
		return []Conflict{{
			Type:    "owner_mismatch",
			Message: "members have different owners; require explicit split or selected family configuration",
			Details: map[string]any{
				"owner_subjects": mapKeys(ownerSubjects),
				"owner_teams":    mapKeys(ownerTeams),
			},
		}}
	}

	// Set the single owner on the proposal.
	for sid := range ownerSubjects {
		v := sid
		prop.OwnerSubjectID = &v
	}
	for tid := range ownerTeams {
		v := tid
		prop.OwnerTeamID = &v
	}
	return nil
}

func detectBucketConflicts(
	members []rawScope,
	bucketByArtifact map[uuid.UUID]uuid.UUID,
	prop *FamilyProposal,
) []Conflict {
	bucketIDs := make(map[uuid.UUID]bool)
	for _, m := range members {
		if bid, ok := bucketByArtifact[m.ArtifactID]; ok {
			bucketIDs[bid] = true
		}
	}

	if len(bucketIDs) > 1 {
		return []Conflict{{
			Type:    "bucket_mismatch",
			Message: "members are mapped to different buckets; require explicit split or selected family configuration",
			Details: mapKeys(bucketIDs),
		}}
	}

	for bid := range bucketIDs {
		v := bid
		prop.BucketID = &v
	}
	return nil
}

func mapKeys(m map[uuid.UUID]bool) []uuid.UUID {
	keys := make([]uuid.UUID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].String() < keys[j].String()
	})
	return keys
}

// --- Database load helpers ---

const dryRunScopesQuery = `
SELECT scope.account_id, scope.artifact_id, scope.kind, scope.name, scope.version, scope.config_key,
       scope.owner_subject_id, scope.owner_team_id,
       CASE WHEN scope.deactivated_at IS NOT NULL THEN 'deactivated' ELSE NULL END,
       state.source_hash,
       state.desired_state->>'kind', state.desired_state->>'name',
       state.desired_state->>'version', state.desired_state->>'language'
FROM fused_artifact_scopes scope
LEFT JOIN fused_config_states state
  ON state.config_key = scope.config_key
 AND state.config_type = scope.kind
 AND state.latest_resource_id = scope.artifact_id
ORDER BY scope.account_id, scope.kind, scope.name, scope.version`

const dryRunTokensQuery = `
SELECT t.id, t.artifact_id, t.token_hash, t.name,
       CASE WHEN t.last_used_at IS NOT NULL THEN t.last_used_at::text END
FROM fused_artifact_tokens t
JOIN fused_artifact_scopes s ON s.artifact_id = t.artifact_id
ORDER BY t.artifact_id, t.name`

const dryRunBucketsQuery = `
SELECT artifact_id, bucket_id
FROM fused_artifact_buckets
ORDER BY artifact_id`

func loadScopes(ctx context.Context, db *pgxpool.Pool) ([]rawScope, error) {
	rows, err := db.Query(ctx, dryRunScopesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var scopes []rawScope
	for rows.Next() {
		var s rawScope
		if err := rows.Scan(&s.AccountID, &s.ArtifactID, &s.Kind,
			&s.Name, &s.Version, &s.ConfigKey,
			&s.OwnerSubjectID, &s.OwnerTeamID, &s.DeactivatedAt,
			&s.SourceHash, &s.DesiredKind, &s.DesiredName,
			&s.DesiredVersion, &s.DesiredLanguage); err != nil {
			return nil, fmt.Errorf("scan scope: %w", err)
		}
		scopes = append(scopes, s)
	}
	return scopes, rows.Err()
}

func loadTokens(ctx context.Context, db *pgxpool.Pool) ([]rawToken, error) {
	rows, err := db.Query(ctx, dryRunTokensQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []rawToken
	for rows.Next() {
		var t rawToken
		if err := rows.Scan(&t.ID, &t.ArtifactID, &t.TokenHash, &t.Name, &t.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

func loadBuckets(ctx context.Context, db *pgxpool.Pool) ([]rawBucket, error) {
	rows, err := db.Query(ctx, dryRunBucketsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []rawBucket
	for rows.Next() {
		var b rawBucket
		if err := rows.Scan(&b.ArtifactID, &b.BucketID); err != nil {
			return nil, fmt.Errorf("scan bucket: %w", err)
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// ToJSON marshals the report to indented JSON.
func (r *DryRunReport) ToJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
