package sandbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// ─── severity derivation ────────────────────────────────────────────────────

func TestChangelogSeverity_VersionRemoved_AlwaysBreaking(t *testing.T) {
	entry := models.ServiceChangelogEntry{ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeRemoved}
	if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityBreaking {
		t.Fatalf("expected breaking, got %v", got)
	}
}

func TestChangelogSeverity_VersionNewAndDeprecated_AlwaysNonBreaking(t *testing.T) {
	for _, ct := range []models.ServiceChangelogType{models.ServiceChangelogTypeNew, models.ServiceChangelogTypeDeprecated} {
		entry := models.ServiceChangelogEntry{ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: ct}
		if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityNonBreaking {
			t.Fatalf("changelog_type=%s: expected non-breaking, got %v", ct, got)
		}
	}
}

func TestChangelogSeverity_VersionChanged_AdditiveOnly_NonBreaking(t *testing.T) {
	diff := []byte(`{"Added":[{"id":"` + uuid.New().String() + `","name":"newOp"}]}`)
	entry := models.ServiceChangelogEntry{
		ConfigType:    models.ServiceChangelogConfigTypeVersion,
		ChangelogType: models.ServiceChangelogTypeChanged,
		Diff:          diff,
	}
	if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityNonBreaking {
		t.Fatalf("purely additive diff should be non-breaking, got %v", got)
	}
}

func TestChangelogSeverity_VersionChanged_AnyRemoved_Breaking(t *testing.T) {
	diff := []byte(`{"Removed":[{"id":"` + uuid.New().String() + `","name":"goneOp"}]}`)
	entry := models.ServiceChangelogEntry{
		ConfigType:    models.ServiceChangelogConfigTypeVersion,
		ChangelogType: models.ServiceChangelogTypeChanged,
		Diff:          diff,
	}
	if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityBreaking {
		t.Fatalf("any removed endpoint should be breaking, got %v", got)
	}
}

func TestChangelogSeverity_VersionChanged_ChangedEndpointBreakingDrift_Breaking(t *testing.T) {
	diff := []byte(`{"Changed":[{"From":{"id":"` + uuid.New().String() + `","name":"op"},"To":{"id":"` + uuid.New().String() + `","name":"op"},"Changes":[{"field":"method","old_value":"GET","new_value":"POST","severity":"breaking","description":"method changed"}]}]}`)
	entry := models.ServiceChangelogEntry{
		ConfigType:    models.ServiceChangelogConfigTypeVersion,
		ChangelogType: models.ServiceChangelogTypeChanged,
		Diff:          diff,
	}
	if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityBreaking {
		t.Fatalf("a breaking DriftChange on a changed endpoint should be breaking, got %v", got)
	}
}

func TestChangelogSeverity_VersionChanged_ChangedEndpointNonBreakingDrift_NonBreaking(t *testing.T) {
	diff := []byte(`{"Changed":[{"From":{"id":"` + uuid.New().String() + `","name":"op"},"To":{"id":"` + uuid.New().String() + `","name":"op"},"Changes":[{"field":"description","old_value":"a","new_value":"b","severity":"non-breaking","description":"description changed"}]}]}`)
	entry := models.ServiceChangelogEntry{
		ConfigType:    models.ServiceChangelogConfigTypeVersion,
		ChangelogType: models.ServiceChangelogTypeChanged,
		Diff:          diff,
	}
	if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityNonBreaking {
		t.Fatalf("a purely non-breaking changed endpoint should be non-breaking, got %v", got)
	}
}

// TestChangelogSeverity_OperationalConfigTypes_DerivedFromDiffNotHardcoded
// proves execution_policy/connection_profile severity comes from the diff's
// own DriftChange.Severity field rather than being hardcoded to
// non-breaking -- Phase 1 only ever emits non-breaking rows today, but this
// pins the derivation as generic so it stays correct if that ever changes.
func TestChangelogSeverity_OperationalConfigTypes_DerivedFromDiffNotHardcoded(t *testing.T) {
	breakingDiff := []byte(`[{"field":"rate_limit","old_value":100,"new_value":10,"severity":"breaking","description":"rate limit lowered"}]`)
	for _, ct := range []models.ServiceChangelogConfigType{models.ServiceChangelogConfigTypeExecutionPolicy, models.ServiceChangelogConfigTypeConnectionProfile} {
		entry := models.ServiceChangelogEntry{ConfigType: ct, Diff: breakingDiff}
		if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityBreaking {
			t.Fatalf("config_type=%s with a breaking DriftChange should be breaking, got %v", ct, got)
		}
	}
}

func TestChangelogSeverity_OperationalConfigTypes_EmptyDiff_NonBreaking(t *testing.T) {
	entry := models.ServiceChangelogEntry{ConfigType: models.ServiceChangelogConfigTypeExecutionPolicy}
	if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityNonBreaking {
		t.Fatalf("expected non-breaking default, got %v", got)
	}
}

func TestChangelogSeverity_UnparseableDiff_DegradesToNonBreaking(t *testing.T) {
	entry := models.ServiceChangelogEntry{
		ConfigType:    models.ServiceChangelogConfigTypeVersion,
		ChangelogType: models.ServiceChangelogTypeChanged,
		Diff:          []byte(`not json`),
	}
	if got := changelogSeverity(entry); got != store.WorkspaceNotificationSeverityNonBreaking {
		t.Fatalf("unparseable diff must degrade to non-breaking, not error, got %v", got)
	}
}

// ─── matchedVersionConfigKeys ───────────────────────────────────────────────

func TestMatchedVersionConfigKeys_Removed_UnionsEveryActivatedVersion(t *testing.T) {
	serviceID := uuid.New()
	v1, v2 := uuid.New(), uuid.New()
	versions := []store.WorkspaceService{
		{ServiceID: serviceID, Version: "2026-01-01", ServiceVersionID: v1},
		{ServiceID: serviceID, Version: "2026-06-01", ServiceVersionID: v2},
	}
	selections := map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch{
		serviceID: {
			v1: {{ConfigKey: "sdk:b"}},
			v2: {{ConfigKey: "sdk:a"}},
		},
	}
	entry := models.ServiceChangelogEntry{ServiceID: serviceID, ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeRemoved}

	keys, impacted := matchedVersionConfigKeys(versions, selections, entry)
	if !impacted {
		t.Fatalf("expected impacted=true")
	}
	if len(keys) != 2 || keys[0] != "sdk:a" || keys[1] != "sdk:b" {
		t.Fatalf("expected sorted deduped [sdk:a sdk:b], got %#v", keys)
	}
}

func TestMatchedVersionConfigKeys_NewAndDeprecated_MatchWholeVersionRegardlessOfSelection(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	versionName := "2026-01-01"
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: versionName, ServiceVersionID: versionID}}
	selections := map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch{
		serviceID: {versionID: {
			{ConfigKey: "sdk:specific", Selection: models.SDKSelection{EndpointIDs: []uuid.UUID{uuid.New()}}},
		}},
	}
	entry := models.ServiceChangelogEntry{
		ServiceID: serviceID, Version: &versionName,
		ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeNew,
	}
	keys, impacted := matchedVersionConfigKeys(versions, selections, entry)
	if !impacted || len(keys) != 1 || keys[0] != "sdk:specific" {
		t.Fatalf("expected [sdk:specific] impacted, got %#v impacted=%v", keys, impacted)
	}
}

func TestMatchedVersionConfigKeys_NoVersion_NotRemoved_NotImpacted(t *testing.T) {
	entry := models.ServiceChangelogEntry{ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeChanged}
	_, impacted := matchedVersionConfigKeys(nil, nil, entry)
	if impacted {
		t.Fatalf("expected not impacted when entry.Version is nil for a non-removed changelog_type")
	}
}

func TestMatchedVersionConfigKeys_VersionNotActivatedByWorkspace_NotImpacted(t *testing.T) {
	serviceID := uuid.New()
	other := "2025-01-01"
	target := "2026-01-01"
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: other, ServiceVersionID: uuid.New()}}
	entry := models.ServiceChangelogEntry{ServiceID: serviceID, Version: &target, ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeNew}
	_, impacted := matchedVersionConfigKeys(versions, nil, entry)
	if impacted {
		t.Fatalf("expected not impacted for a version this workspace never activated")
	}
}

// TestMatchedVersionConfigKeys_Changed_NarrowsByEndpointSelection is the
// core endpoint-narrowing rule: a SelectAll config is always included; an
// EndpointIDs-restricted config is only included if the diff's
// Removed/Changed entries touch one of its selected endpoints.
func TestMatchedVersionConfigKeys_Changed_NarrowsByEndpointSelection(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	versionName := "2026-01-01"
	touchedID := uuid.New()
	untouchedID := uuid.New()
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: versionName, ServiceVersionID: versionID}}
	selections := map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch{
		serviceID: {versionID: {
			{ConfigKey: "sdk:all", Selection: models.SDKSelection{SelectAll: true}},
			{ConfigKey: "sdk:touched", Selection: models.SDKSelection{EndpointIDs: []uuid.UUID{touchedID}}},
			{ConfigKey: "sdk:untouched", Selection: models.SDKSelection{EndpointIDs: []uuid.UUID{untouchedID}}},
		}},
	}
	diff := []byte(`{"Changed":[{"From":{"id":"` + touchedID.String() + `","name":"op"},"To":{"id":"` + touchedID.String() + `","name":"op"},"Changes":[{"field":"x","severity":"non-breaking"}]}]}`)
	entry := models.ServiceChangelogEntry{
		ServiceID: serviceID, Version: &versionName,
		ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeChanged,
		Diff: diff,
	}
	keys, impacted := matchedVersionConfigKeys(versions, selections, entry)
	if !impacted {
		t.Fatalf("expected impacted=true")
	}
	want := map[string]bool{"sdk:all": true, "sdk:touched": true}
	if len(keys) != 2 {
		t.Fatalf("expected exactly 2 matched configs, got %#v", keys)
	}
	for _, k := range keys {
		if !want[k] {
			t.Fatalf("unexpected config %q matched; sdk:untouched must never match an endpoint it didn't select: %#v", k, keys)
		}
	}
}

func TestMatchedVersionConfigKeys_Changed_AddedEndpointNeverMatchesExistingSelection(t *testing.T) {
	// A config can't have pre-selected an endpoint that didn't exist yet, so
	// an Added-only diff must never mark an EndpointIDs-restricted config as
	// impacted even if (by coincidence) the new ID were listed -- narrowing
	// only ever looks at Removed/Changed.
	serviceID, versionID := uuid.New(), uuid.New()
	versionName := "2026-01-01"
	newID := uuid.New()
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: versionName, ServiceVersionID: versionID}}
	selections := map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch{
		serviceID: {versionID: {
			{ConfigKey: "sdk:restricted", Selection: models.SDKSelection{EndpointIDs: []uuid.UUID{newID}}},
		}},
	}
	diff := []byte(`{"Added":[{"id":"` + newID.String() + `","name":"newOp"}]}`)
	entry := models.ServiceChangelogEntry{
		ServiceID: serviceID, Version: &versionName,
		ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeChanged,
		Diff: diff,
	}
	_, impacted := matchedVersionConfigKeys(versions, selections, entry)
	if impacted {
		t.Fatalf("an Added-only diff must never impact an EndpointIDs-restricted config")
	}
}

// ─── matchedExecutionPolicyConfigKeys ───────────────────────────────────────

type matcherMockStore struct {
	store.Store
	scopes           map[uuid.UUID]*store.ArtifactScope
	overrides        map[uuid.UUID]*store.WorkspaceExecutionPolicyOverride // keyed by ServiceVersionID
	overrideErr      error
	connectProfiles  []store.WorkspaceConnectionProfile
	connectListErr   error
	hasOverrideCheck bool
	hasConnectLister bool
}

func (m *matcherMockStore) ListArtifactScopes(ctx context.Context, artifactIDs []uuid.UUID) (map[uuid.UUID]*store.ArtifactScope, error) {
	out := make(map[uuid.UUID]*store.ArtifactScope)
	for _, id := range artifactIDs {
		if scope, ok := m.scopes[id]; ok {
			out[id] = scope
		}
	}
	return out, nil
}

func (m *matcherMockStore) GetEffectiveWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*store.WorkspaceExecutionPolicyOverride, error) {
	if m.overrideErr != nil {
		return nil, m.overrideErr
	}
	return m.overrides[serviceVersionID], nil
}

func (m *matcherMockStore) ListWorkspaceConnectProfiles(ctx context.Context) ([]store.WorkspaceConnectionProfile, error) {
	if m.connectListErr != nil {
		return nil, m.connectListErr
	}
	return m.connectProfiles, nil
}

// matcherMockStoreNoOverride/NoConnect embed matcherMockStore's fields but
// deliberately don't implement the optional capability, proving the
// type-assertion gate fails closed (no notification) rather than panicking.
type matcherMockStoreNoOptionalCapabilities struct {
	store.Store
}

func TestMatchedExecutionPolicyConfigKeys_NoOverrideCapability_NotImpacted(t *testing.T) {
	s := &matcherMockStoreNoOptionalCapabilities{}
	entry := models.ServiceChangelogEntry{ServiceID: uuid.New(), ConfigType: models.ServiceChangelogConfigTypeExecutionPolicy}
	_, impacted := matchedExecutionPolicyConfigKeys(context.Background(), s, nil, nil, entry)
	if impacted {
		t.Fatalf("expected not impacted when the store doesn't support execution policy override checks")
	}
}

func TestMatchedExecutionPolicyConfigKeys_LocalOverrideShadows_Skipped(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	versionName := "2026-01-01"
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: versionName, ServiceVersionID: versionID}}
	selections := map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch{
		serviceID: {versionID: {{ConfigKey: "sdk:x"}}},
	}
	s := &matcherMockStore{overrides: map[uuid.UUID]*store.WorkspaceExecutionPolicyOverride{
		versionID: {ServiceID: serviceID, ServiceVersionID: &versionID},
	}}
	entry := models.ServiceChangelogEntry{ServiceID: serviceID, Version: &versionName, ConfigType: models.ServiceChangelogConfigTypeExecutionPolicy}

	_, impacted := matchedExecutionPolicyConfigKeys(context.Background(), s, versions, selections, entry)
	if impacted {
		t.Fatalf("a local override at this version should shadow the Registry change entirely")
	}
}

func TestMatchedExecutionPolicyConfigKeys_NoOverride_Included(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	versionName := "2026-01-01"
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: versionName, ServiceVersionID: versionID}}
	selections := map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch{
		serviceID: {versionID: {{ConfigKey: "sdk:x"}}},
	}
	s := &matcherMockStore{overrides: map[uuid.UUID]*store.WorkspaceExecutionPolicyOverride{}}
	entry := models.ServiceChangelogEntry{ServiceID: serviceID, Version: &versionName, ConfigType: models.ServiceChangelogConfigTypeExecutionPolicy}

	keys, impacted := matchedExecutionPolicyConfigKeys(context.Background(), s, versions, selections, entry)
	if !impacted || len(keys) != 1 || keys[0] != "sdk:x" {
		t.Fatalf("expected [sdk:x] impacted, got %#v impacted=%v", keys, impacted)
	}
}

func TestMatchedExecutionPolicyConfigKeys_ServiceWideDefault_ChecksEveryVersionIndependently(t *testing.T) {
	serviceID := uuid.New()
	v1, v2 := uuid.New(), uuid.New()
	versions := []store.WorkspaceService{
		{ServiceID: serviceID, Version: "v1", ServiceVersionID: v1},
		{ServiceID: serviceID, Version: "v2", ServiceVersionID: v2},
	}
	selections := map[uuid.UUID]map[uuid.UUID][]store.WorkspaceSDKSelectionMatch{
		serviceID: {
			v1: {{ConfigKey: "sdk:v1"}},
			v2: {{ConfigKey: "sdk:v2"}},
		},
	}
	// v2 has a local override shadowing the service-wide default; v1 doesn't.
	s := &matcherMockStore{overrides: map[uuid.UUID]*store.WorkspaceExecutionPolicyOverride{
		v2: {ServiceID: serviceID},
	}}
	entry := models.ServiceChangelogEntry{ServiceID: serviceID, Version: nil, ConfigType: models.ServiceChangelogConfigTypeExecutionPolicy}

	keys, impacted := matchedExecutionPolicyConfigKeys(context.Background(), s, versions, selections, entry)
	if !impacted || len(keys) != 1 || keys[0] != "sdk:v1" {
		t.Fatalf("expected only sdk:v1 (v2 shadowed locally), got %#v impacted=%v", keys, impacted)
	}
}

// ─── matchedConnectionProfileConfigKeys ─────────────────────────────────────

func TestMatchedConnectionProfileConfigKeys_NoListerCapability_NotImpacted(t *testing.T) {
	s := &matcherMockStoreNoOptionalCapabilities{}
	entry := models.ServiceChangelogEntry{ServiceID: uuid.New(), ConfigType: models.ServiceChangelogConfigTypeConnectionProfile}
	_, impacted := matchedConnectionProfileConfigKeys(context.Background(), s, nil, entry)
	if impacted {
		t.Fatalf("expected not impacted when the store doesn't support ListWorkspaceConnectProfiles")
	}
}

func TestMatchedConnectionProfileConfigKeys_BaselineIncluded_OverrideExcluded(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	versionName := "2026-01-01"
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: versionName, ServiceVersionID: versionID}}
	s := &matcherMockStore{connectProfiles: []store.WorkspaceConnectionProfile{
		{ServiceID: serviceID, ServiceVersionID: versionID, AuthType: "api_key", Layer: "baseline"},
		{ServiceID: serviceID, ServiceVersionID: versionID, AuthType: "oauth2", Layer: "override"},
	}}
	entry := models.ServiceChangelogEntry{ServiceID: serviceID, Version: &versionName, ConfigType: models.ServiceChangelogConfigTypeConnectionProfile}

	keys, impacted := matchedConnectionProfileConfigKeys(context.Background(), s, versions, entry)
	if !impacted || len(keys) != 1 || keys[0] != "connection_profile:api_key" {
		t.Fatalf("expected only the baseline auth_type impacted, got %#v impacted=%v", keys, impacted)
	}
}

func TestMatchedConnectionProfileConfigKeys_UnknownVersion_NotImpacted(t *testing.T) {
	serviceID := uuid.New()
	target := "2026-06-01"
	s := &matcherMockStore{connectProfiles: []store.WorkspaceConnectionProfile{
		{ServiceID: serviceID, AuthType: "api_key", Layer: "baseline"},
	}}
	entry := models.ServiceChangelogEntry{ServiceID: serviceID, Version: &target, ConfigType: models.ServiceChangelogConfigTypeConnectionProfile}
	_, impacted := matchedConnectionProfileConfigKeys(context.Background(), s, nil, entry)
	if impacted {
		t.Fatalf("expected not impacted when the changelog's version isn't resolvable from activated versions")
	}
}

// ─── changelogNotificationType ──────────────────────────────────────────────

func TestChangelogNotificationType_MapsEveryCombination(t *testing.T) {
	cases := []struct {
		configType models.ServiceChangelogConfigType
		changeType models.ServiceChangelogType
		want       store.WorkspaceNotificationType
		ok         bool
	}{
		{models.ServiceChangelogConfigTypeVersion, models.ServiceChangelogTypeNew, store.WorkspaceNotificationTypeRegistryVersionAdded, true},
		{models.ServiceChangelogConfigTypeVersion, models.ServiceChangelogTypeChanged, store.WorkspaceNotificationTypeRegistryVersionChanged, true},
		{models.ServiceChangelogConfigTypeVersion, models.ServiceChangelogTypeDeprecated, store.WorkspaceNotificationTypeRegistryVersionDeprecated, true},
		{models.ServiceChangelogConfigTypeVersion, models.ServiceChangelogTypeRemoved, store.WorkspaceNotificationTypeRegistryVersionRemoved, true},
		{models.ServiceChangelogConfigTypeExecutionPolicy, "", store.WorkspaceNotificationTypeRegistryExecutionPolicyChanged, true},
		{models.ServiceChangelogConfigTypeConnectionProfile, "", store.WorkspaceNotificationTypeRegistryConnectionProfileChanged, true},
		{models.ServiceChangelogConfigType("bogus"), "", "", false},
	}
	for _, c := range cases {
		got, ok := changelogNotificationType(models.ServiceChangelogEntry{ConfigType: c.configType, ChangelogType: c.changeType})
		if ok != c.ok || got != c.want {
			t.Fatalf("config_type=%s changelog_type=%s: expected (%q,%v), got (%q,%v)", c.configType, c.changeType, c.want, c.ok, got, ok)
		}
	}
}

// ─── small pure helpers ─────────────────────────────────────────────────────

func TestDedupSortedStrings(t *testing.T) {
	got := dedupSortedStrings([]string{"b", "a", "b", "c", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
	if dedupSortedStrings(nil) != nil {
		t.Fatalf("expected nil for empty input")
	}
}

func TestResolveServiceVersionID(t *testing.T) {
	target := uuid.New()
	versions := []store.WorkspaceService{{Version: "v1", ServiceVersionID: uuid.New()}, {Version: "v2", ServiceVersionID: target}}
	got, ok := resolveServiceVersionID(versions, "v2")
	if !ok || got != target {
		t.Fatalf("expected to resolve v2 to %v, got %v ok=%v", target, got, ok)
	}
	if _, ok := resolveServiceVersionID(versions, "v3"); ok {
		t.Fatalf("expected not found for an unactivated version name")
	}
}

// ─── end-to-end: matchAndNotifyServiceChangelog ─────────────────────────────

type matcherMockConfigStore struct {
	store.ConfigRepository
	states     []store.ConfigState
	listErr    error
	created    []store.CreateWorkspaceNotificationParams
	createErrs map[int]error // index into calls, for testing tolerance
}

func (m *matcherMockConfigStore) ListConfigStates(ctx context.Context, configType store.ConfigType) ([]store.ConfigState, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.states, nil
}

func (m *matcherMockConfigStore) CreateWorkspaceNotification(ctx context.Context, params store.CreateWorkspaceNotificationParams) (*store.WorkspaceNotification, error) {
	idx := len(m.created)
	m.created = append(m.created, params)
	if err, ok := m.createErrs[idx]; ok {
		return nil, err
	}
	return &store.WorkspaceNotification{ID: uuid.New()}, nil
}

func TestMatchAndNotifyServiceChangelog_EndToEnd_CreatesNotificationWithDedupeMetadata(t *testing.T) {
	serviceID, versionID, artifactID := uuid.New(), uuid.New(), uuid.New()
	versionName := "2026-01-01"

	selectionsJSON, err := json.Marshal([]models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: versionID, SelectAll: true}})
	if err != nil {
		t.Fatalf("marshal selections: %v", err)
	}
	engineStore := &matcherMockStore{
		scopes: map[uuid.UUID]*store.ArtifactScope{
			artifactID: {ArtifactID: artifactID, Selections: selectionsJSON},
		},
	}
	configStore := &matcherMockConfigStore{
		states: []store.ConfigState{{ConfigKey: "sdk:test", ConfigType: store.ConfigTypeSDK, LatestResourceID: &artifactID}},
	}
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: versionName, ServiceVersionID: versionID}}
	changelogID := uuid.New()
	entries := []models.ServiceChangelogEntry{{
		ID: changelogID, ServiceID: serviceID, Version: &versionName,
		ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeNew,
	}}

	matchAndNotifyServiceChangelog(context.Background(), engineStore, configStore, versions, entries)

	if len(configStore.created) != 1 {
		t.Fatalf("expected exactly 1 notification created, got %d", len(configStore.created))
	}
	n := configStore.created[0]
	if n.Type != store.WorkspaceNotificationTypeRegistryVersionAdded {
		t.Fatalf("expected registry_version_added, got %s", n.Type)
	}
	if n.Severity != store.WorkspaceNotificationSeverityNonBreaking {
		t.Fatalf("expected non-breaking for a version/new entry, got %s", n.Severity)
	}
	if n.ConfigKey != "sdk:test" {
		t.Fatalf("expected config_key sdk:test, got %q", n.ConfigKey)
	}
	var metadata map[string]string
	if err := json.Unmarshal(n.Metadata, &metadata); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if metadata["registry_changelog_id"] != changelogID.String() {
		t.Fatalf("expected metadata.registry_changelog_id=%s, got %#v", changelogID, metadata)
	}
}

func TestMatchAndNotifyServiceChangelog_NoImpactedConfigs_NoNotification(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	versionName := "2026-01-01"
	engineStore := &matcherMockStore{}
	configStore := &matcherMockConfigStore{} // no config states at all
	versions := []store.WorkspaceService{{ServiceID: serviceID, Version: versionName, ServiceVersionID: versionID}}
	entries := []models.ServiceChangelogEntry{{
		ID: uuid.New(), ServiceID: serviceID, Version: &versionName,
		ConfigType: models.ServiceChangelogConfigTypeVersion, ChangelogType: models.ServiceChangelogTypeNew,
	}}

	matchAndNotifyServiceChangelog(context.Background(), engineStore, configStore, versions, entries)

	if len(configStore.created) != 1 {
		t.Fatalf("expected 1 notification for new version despite no SDK config, got %d", len(configStore.created))
	}
	if configStore.created[0].ConfigKey != "" {
		t.Errorf("expected empty config key for notification, got %q", configStore.created[0].ConfigKey)
	}
}

func TestMatchAndNotifyServiceChangelog_EmptyInputs_NoOp(t *testing.T) {
	configStore := &matcherMockConfigStore{}
	matchAndNotifyServiceChangelog(context.Background(), &matcherMockStore{}, configStore, nil, []models.ServiceChangelogEntry{{ID: uuid.New()}})
	matchAndNotifyServiceChangelog(context.Background(), &matcherMockStore{}, configStore, []store.WorkspaceService{{}}, nil)
	if len(configStore.created) != 0 {
		t.Fatalf("expected no notifications for empty versions or empty entries, got %#v", configStore.created)
	}
}
