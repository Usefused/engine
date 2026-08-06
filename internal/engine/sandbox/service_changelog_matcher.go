package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// versionDiffWire mirrors the Registry drift JSON shape without importing
// Registry-internal packages. EndpointVersionDiff/EndpointVersionChange have no json
// tags of their own, so Go's default marshaling capitalizes their keys
// (Changed/Added/Removed/From/To/Changes); the nested models.IntegrationObject
// and models.DriftChange values do have tags and are shared
// (internal/shared/models), so they're reused directly rather than
// duplicated.
type versionDiffWire struct {
	Changed []struct {
		From    models.IntegrationObject `json:"From"`
		To      models.IntegrationObject `json:"To"`
		Changes []models.DriftChange     `json:"Changes"`
	} `json:"Changed"`
	Added   []models.IntegrationObject `json:"Added"`
	Removed []models.IntegrationObject `json:"Removed"`
}

// changelogSeverity derives a WorkspaceNotificationSeverity from a single
// service_changelog entry by inspecting its diff payload. Execution policy
// and connection profile diffs are analyzed for breaking changes; version
// diffs are compared against the currently loaded service contract. Any
// unmarshal failure degrades to non-breaking — the conservative default
// that never over-alarms — rather than failing the whole match.
func changelogSeverity(entry models.ServiceChangelogEntry) store.WorkspaceNotificationSeverity {
	switch entry.ConfigType {
	case models.ServiceChangelogConfigTypeExecutionPolicy, models.ServiceChangelogConfigTypeConnectionProfile:
		return operationalDiffSeverity(entry.Diff)
	case models.ServiceChangelogConfigTypeVersion:
		return versionChangelogSeverity(entry)
	default:
		return store.WorkspaceNotificationSeverityNonBreaking
	}
}

// operationalDiffSeverity handles execution_policy/connection_profile,
// whose diff payload is always []models.DriftChange directly (see
// DiffExecutionPolicy/DiffConnectionProfile). As of Phase 1 these always
// emit "non-breaking" (see diffPtrField's own doc comment), so this will
// always resolve non-breaking today -- written generically off the diff
// rather than hard-coded so it stays correct if that ever changes upstream.
func operationalDiffSeverity(diff json.RawMessage) store.WorkspaceNotificationSeverity {
	if len(diff) == 0 {
		return store.WorkspaceNotificationSeverityNonBreaking
	}
	var changes []models.DriftChange
	if err := json.Unmarshal(diff, &changes); err != nil {
		return store.WorkspaceNotificationSeverityNonBreaking
	}
	return severityFromDriftChanges(changes)
}

// versionChangelogSeverity handles config_type=version's four
// changelog_types: removed is always breaking (nothing to diff -- the
// whole service is gone); new and deprecated are non-breaking (nothing
// existing broke yet); changed defers to the endpoint diff itself.
func versionChangelogSeverity(entry models.ServiceChangelogEntry) store.WorkspaceNotificationSeverity {
	switch entry.ChangelogType {
	case models.ServiceChangelogTypeRemoved:
		return store.WorkspaceNotificationSeverityBreaking
	case models.ServiceChangelogTypeChanged:
		return endpointVersionDiffSeverity(entry.Diff)
	default: // new, deprecated
		return store.WorkspaceNotificationSeverityNonBreaking
	}
}

// endpointVersionDiffSeverity: breaking if anything was removed, or if any
// changed endpoint's own DriftChange entries say breaking; a purely
// additive diff (new endpoints only) is non-breaking.
func endpointVersionDiffSeverity(diff json.RawMessage) store.WorkspaceNotificationSeverity {
	if len(diff) == 0 {
		return store.WorkspaceNotificationSeverityNonBreaking
	}
	var wire versionDiffWire
	if err := json.Unmarshal(diff, &wire); err != nil {
		return store.WorkspaceNotificationSeverityNonBreaking
	}
	if len(wire.Removed) > 0 {
		return store.WorkspaceNotificationSeverityBreaking
	}
	for _, changed := range wire.Changed {
		if severityFromDriftChanges(changed.Changes) == store.WorkspaceNotificationSeverityBreaking {
			return store.WorkspaceNotificationSeverityBreaking
		}
	}
	return store.WorkspaceNotificationSeverityNonBreaking
}

func severityFromDriftChanges(changes []models.DriftChange) store.WorkspaceNotificationSeverity {
	for _, change := range changes {
		if change.Severity == "breaking" {
			return store.WorkspaceNotificationSeverityBreaking
		}
	}
	return store.WorkspaceNotificationSeverityNonBreaking
}

// matchAndNotifyServiceChangelog is the entry point for changelog-driven
// workspace notifications, called from the poller right after
// InsertServiceChangelogCacheEntries succeeds (see service_changelog_poller.go). versions is this service's activated
// version rows (from ListWorkspaceServices, grouped by ServiceID) -- needed
// to resolve a changelog entry's version *name* to the ServiceVersionID the
// usage index and execution-policy/profile checks are keyed by.
func matchAndNotifyServiceChangelog(ctx context.Context, engineStore store.Store, configStore store.ConfigRepository, versions []store.WorkspaceService, entries []models.ServiceChangelogEntry) {
	if len(entries) == 0 || len(versions) == 0 {
		return
	}
	selections, err := store.WorkspaceSDKSelectionsByServiceVersion(ctx, configStore, engineStore)
	if err != nil {
		// Best-effort, matching this whole feature's established tolerance:
		// a usage-index failure skips matching for this tick's entries
		// rather than blocking the cache write/cursor advance that already
		// succeeded. Those specific rows won't be re-matched later (the
		// cursor has moved past them) -- an accepted gap, the same class of
		// trade-off Phase 1/2 already make for their own best-effort steps.
		slog.ErrorContext(ctx, "changelog matcher failed to build usage index, skipping matching this tick", slog.Any("error", err))
		return
	}
	for _, entry := range entries {
		notifyIfImpacted(ctx, engineStore, configStore, versions, selections, entry)
	}
}

func notifyIfImpacted(
	ctx context.Context,
	engineStore store.Store,
	configStore store.ConfigRepository,
	versions []store.WorkspaceService,
	selections map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch,
	entry models.ServiceChangelogEntry,
) {
	configKeys, impacted := matchedConfigKeys(ctx, engineStore, versions, selections, entry)
	if !impacted {
		return
	}
	notifyType, ok := changelogNotificationType(entry)
	if !ok {
		return
	}
	createChangelogNotification(ctx, configStore, entry, notifyType, configKeys)
}

// matchedConfigKeys dispatches to the per-config_type matching rule:
// version reuses the SDK usage index (narrowed by endpoint selection for
// `changed`); execution_policy is ambient, gated only by whether a local
// override already shadows it; connection_profile is gated by whether the
// workspace's effective profile is still on the baseline layer.
func matchedConfigKeys(
	ctx context.Context,
	engineStore store.Store,
	versions []store.WorkspaceService,
	selections map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch,
	entry models.ServiceChangelogEntry,
) ([]string, bool) {
	switch entry.ConfigType {
	case models.ServiceChangelogConfigTypeVersion:
		return matchedVersionConfigKeys(versions, selections, entry)
	case models.ServiceChangelogConfigTypeExecutionPolicy:
		return matchedExecutionPolicyConfigKeys(ctx, engineStore, versions, selections, entry)
	case models.ServiceChangelogConfigTypeConnectionProfile:
		return matchedConnectionProfileConfigKeys(ctx, engineStore, versions, entry)
	default:
		return nil, false
	}
}

// matchedVersionConfigKeys: `removed` is service granularity (Version nil)
// -- every activated version of this service is impacted. `new`/`deprecated`
// match at the coarse service+version level (the whole point of either
// event affects every config on that version, regardless of which specific
// endpoints it selected). `changed` narrows further by endpoint selection.
func matchedVersionConfigKeys(versions []store.WorkspaceService, selections map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch, entry models.ServiceChangelogEntry) ([]string, bool) {
	if entry.ChangelogType == models.ServiceChangelogTypeRemoved {
		var keys []string
		for _, v := range versions {
			keys = append(keys, configKeysForVersion(selections, entry.ServiceID, v.ServiceVersionID)...)
		}
		return dedupSortedStrings(keys), len(keys) > 0
	}
	if entry.Version == nil {
		return nil, false
	}
	versionID, ok := resolveServiceVersionID(versions, *entry.Version)
	if !ok {
		// This workspace has no activated row for this exact version --
		// nothing to notify about it.
		return nil, false
	}
	matches := selections[entry.ServiceID][versionID]
	if entry.ChangelogType != models.ServiceChangelogTypeChanged {
		// new/deprecated: the version being activated in this workspace is
		// sufficient to be impacted -- no SDK selection required.  An empty
		// matches slice just means no SDK config yet; configKeys will be
		// empty but the notification is still meaningful to the operator.
		return dedupSortedStrings(configKeysFromMatches(matches)), true
	}
	if len(matches) == 0 {
		return nil, false
	}
	return narrowByEndpointDiff(matches, entry.Diff)

}

// narrowByEndpointDiff filters matches down to configs actually affected by
// a version/changed diff: a SelectAll config is always included (it tracks
// every endpoint automatically); a config with an explicit EndpointIDs/
// OperationNames list is included only if the diff's Removed or Changed
// entries touch one of its selected endpoints -- never Added, since a
// config can't have pre-selected an endpoint that didn't exist yet.
func narrowByEndpointDiff(matches []store.WorkspaceSDKSelectionMatch, diff json.RawMessage) ([]string, bool) {
	changedIDs, changedNames := changedOrRemovedEndpoints(diff)
	var keys []string
	for _, match := range matches {
		if match.Selection.SelectAll || selectionTouchesEndpoints(match.Selection, changedIDs, changedNames) {
			keys = append(keys, match.ConfigKey)
		}
	}
	return dedupSortedStrings(keys), len(keys) > 0
}

func changedOrRemovedEndpoints(diff json.RawMessage) (map[uuid.UUID]bool, map[string]bool) {
	ids := map[uuid.UUID]bool{}
	names := map[string]bool{}
	if len(diff) == 0 {
		return ids, names
	}
	var wire versionDiffWire
	if err := json.Unmarshal(diff, &wire); err != nil {
		return ids, names
	}
	for _, removed := range wire.Removed {
		ids[removed.ID] = true
		names[removed.Name] = true
	}
	for _, changed := range wire.Changed {
		ids[changed.From.ID] = true
		names[changed.From.Name] = true
	}
	return ids, names
}

func selectionTouchesEndpoints(selection models.SDKSelection, ids map[uuid.UUID]bool, names map[string]bool) bool {
	for _, id := range selection.EndpointIDs {
		if ids[id] {
			return true
		}
	}
	for _, name := range selection.OperationNames {
		if names[name] {
			return true
		}
	}
	return false
}

// executionPolicyOverrideChecker is the optional capability
// matchedExecutionPolicyConfigKeys needs -- the same anonymous-interface
// type-assertion idiom cache.go's applyExecutionPolicyOverride already uses
// for this exact method.
type executionPolicyOverrideChecker interface {
	GetEffectiveWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*store.WorkspaceExecutionPolicyOverride, error)
}

// matchedExecutionPolicyConfigKeys: execution_policy is ambient (ignores
// the SDK usage index for *gating* -- every dispatch on the version is
// subject to it -- but still reuses it to report which configs will feel
// the change). A local override at the matching (service, version) tier
// shadows the Registry default entirely, so that version is skipped, no
// notification. entry.Version nil means the service-wide default changed,
// so every activated version is checked independently, since each one
// resolves its own override-if-present-else-service-default precedence.
func matchedExecutionPolicyConfigKeys(ctx context.Context, engineStore store.Store, versions []store.WorkspaceService, selections map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch, entry models.ServiceChangelogEntry) ([]string, bool) {
	overrideStore, ok := engineStore.(executionPolicyOverrideChecker)
	if !ok {
		return nil, false
	}
	var keys []string
	for _, v := range versions {
		if entry.Version != nil && v.Version != *entry.Version {
			continue
		}
		override, err := overrideStore.GetEffectiveWorkspaceExecutionPolicyOverride(ctx, entry.ServiceID, v.ServiceVersionID)
		if err != nil {
			slog.ErrorContext(ctx, "changelog matcher failed to check execution policy override",
				slog.Any("error", err), slog.String("service_version_id", v.ServiceVersionID.String()))
			continue
		}
		if override != nil {
			continue // shadowed locally -- this Registry change is moot for this version
		}
		keys = append(keys, configKeysForVersion(selections, entry.ServiceID, v.ServiceVersionID)...)
	}
	return dedupSortedStrings(keys), len(keys) > 0
}

// connectProfileLister is the optional capability matchedConnectionProfileConfigKeys
// needs -- WorkspaceConnectSyncStore in store.go.
type connectProfileLister interface {
	ListWorkspaceConnectProfiles(ctx context.Context) ([]store.WorkspaceConnectionProfile, error)
}

// matchedConnectionProfileConfigKeys: a connection_profile changelog row
// doesn't record which auth_type changed (the schema has no such column,
// so rather than guessing, this checks every effective profile the
// workspace holds for this (service, version): any of them still on the
// baseline layer means the workspace inherits whatever Registry default
// just changed for that auth_type, so it's notified about that auth_type
// specifically (an override-layer profile is unaffected -- the workspace's
// own override already supersedes the baseline revision that changed).
func matchedConnectionProfileConfigKeys(ctx context.Context, engineStore store.Store, versions []store.WorkspaceService, entry models.ServiceChangelogEntry) ([]string, bool) {
	lister, ok := engineStore.(connectProfileLister)
	if !ok {
		return nil, false
	}
	profiles, err := lister.ListWorkspaceConnectProfiles(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "changelog matcher failed to list workspace connect profiles", slog.Any("error", err))
		return nil, false
	}
	var versionID uuid.UUID
	if entry.Version != nil {
		id, ok := resolveServiceVersionID(versions, *entry.Version)
		if !ok {
			return nil, false
		}
		versionID = id
	}
	var affected []string
	for _, profile := range profiles {
		if profile.ServiceID != entry.ServiceID || profile.Layer != "baseline" {
			continue
		}
		if entry.Version != nil && profile.ServiceVersionID != versionID {
			continue
		}
		affected = append(affected, "connection_profile:"+profile.AuthType)
	}
	return dedupSortedStrings(affected), len(affected) > 0
}

func configKeysForVersion(selections map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch, serviceID, versionID uuid.UUID) []string {
	return configKeysFromMatches(selections[serviceID][versionID])
}

func configKeysFromMatches(matches []store.WorkspaceSDKSelectionMatch) []string {
	keys := make([]string, len(matches))
	for i, match := range matches {
		keys[i] = match.ConfigKey
	}
	return keys
}

func resolveServiceVersionID(versions []store.WorkspaceService, versionName string) (uuid.UUID, bool) {
	for _, v := range versions {
		if v.Version == versionName {
			return v.ServiceVersionID, true
		}
	}
	return uuid.Nil, false
}

func dedupSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// changelogNotificationType maps a service_changelog entry to one of the
// registry_* notification types used for changelog-driven workspace
// notifications (see config_repository.go).
func changelogNotificationType(entry models.ServiceChangelogEntry) (store.WorkspaceNotificationType, bool) {
	switch entry.ConfigType {
	case models.ServiceChangelogConfigTypeVersion:
		switch entry.ChangelogType {
		case models.ServiceChangelogTypeNew:
			return store.WorkspaceNotificationTypeRegistryVersionAdded, true
		case models.ServiceChangelogTypeChanged:
			return store.WorkspaceNotificationTypeRegistryVersionChanged, true
		case models.ServiceChangelogTypeDeprecated:
			return store.WorkspaceNotificationTypeRegistryVersionDeprecated, true
		case models.ServiceChangelogTypeRemoved:
			return store.WorkspaceNotificationTypeRegistryVersionRemoved, true
		default:
			return "", false
		}
	case models.ServiceChangelogConfigTypeExecutionPolicy:
		return store.WorkspaceNotificationTypeRegistryExecutionPolicyChanged, true
	case models.ServiceChangelogConfigTypeConnectionProfile:
		return store.WorkspaceNotificationTypeRegistryConnectionProfileChanged, true
	default:
		return "", false
	}
}

// createChangelogNotification builds and inserts one notification for a
// matched changelog entry. Dedupe is by registry_changelog_id (see
// config_repository.go's workspaceNotificationDedupeMatchSQL), so this is
// safe to call again for the same entry across ticks/crashes -- one
// notification per changelog row regardless of how many configs it
// impacts, mirroring the existing removal flow's
// createWorkspaceRemovalNotificationsForAction.
func createChangelogNotification(ctx context.Context, configStore store.ConfigRepository, entry models.ServiceChangelogEntry, notifyType store.WorkspaceNotificationType, configKeys []string) {
	metadata, err := json.Marshal(map[string]string{"registry_changelog_id": entry.ID.String()})
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal changelog notification metadata", slog.Any("error", err), slog.String("registry_changelog_id", entry.ID.String()))
		return
	}
	serviceID := entry.ServiceID
	version := ""
	if entry.Version != nil {
		version = *entry.Version
	}
	_, err = configStore.CreateWorkspaceNotification(ctx, store.CreateWorkspaceNotificationParams{
		Type:      notifyType,
		Severity:  changelogSeverity(entry),
		ServiceID: &serviceID,
		Version:   version,
		ConfigKey: strings.Join(configKeys, ", "),
		Message:   changelogNotificationMessage(entry, notifyType, configKeys),
		Metadata:  metadata,
		// CreatedBy is uuid.Nil: this is a system-generated discovery, not
		// a specific account's action -- the column has no FK/NOT NULL
		// constraint, so this is schema-safe (see schema_engine.go).
		CreatedBy: uuid.Nil,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to create changelog notification", slog.Any("error", err), slog.String("registry_changelog_id", entry.ID.String()))
	}
}

func changelogNotificationMessage(entry models.ServiceChangelogEntry, notifyType store.WorkspaceNotificationType, configKeys []string) string {
	version := "the service"
	if entry.Version != nil {
		version = fmt.Sprintf("version %s", *entry.Version)
	}
	switch notifyType {
	case store.WorkspaceNotificationTypeRegistryVersionAdded:
		return fmt.Sprintf("A new %s was published for a service this workspace uses.", version)
	case store.WorkspaceNotificationTypeRegistryVersionChanged:
		return fmt.Sprintf("%s changed endpoints, affecting %d of your configs.", firstUpper(version), len(configKeys))
	case store.WorkspaceNotificationTypeRegistryVersionDeprecated:
		return fmt.Sprintf("%s was deprecated; it still works today but will eventually be removed.", firstUpper(version))
	case store.WorkspaceNotificationTypeRegistryVersionRemoved:
		return "The service was removed from the Registry while your configs still reference it."
	case store.WorkspaceNotificationTypeRegistryExecutionPolicyChanged:
		return fmt.Sprintf("The execution policy for %s changed and this workspace has no local override.", version)
	case store.WorkspaceNotificationTypeRegistryConnectionProfileChanged:
		return fmt.Sprintf("A connection profile baseline for %s changed and this workspace inherits it directly.", version)
	default:
		return "A service this workspace uses was changed on the Registry."
	}
}

// firstUpper capitalizes only the first rune -- strings.Title is deprecated
// and would title-case every word, which isn't wanted here ("Version 1.0"
// not "Version 1.0" vs "The Service").
func firstUpper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
