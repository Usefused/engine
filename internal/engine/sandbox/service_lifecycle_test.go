package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type lifecycleSandboxStore struct {
	changelogPollerMockStore
	deleted  bool
	applyErr error
	applied  int
}

// IsServiceDeleted models the durable gate independently of a warm object cache.
func (s *lifecycleSandboxStore) IsServiceDeleted(context.Context, uuid.UUID) (bool, error) {
	return s.deleted, nil
}

// ApplyServiceLifecycle records compaction attempts, including recoverable storage failures.
func (s *lifecycleSandboxStore) ApplyServiceLifecycle(context.Context, uuid.UUID, string, *time.Time, bool, time.Time) error {
	s.applied++
	return s.applyErr
}

// TestDeletedServiceEvictsWarmOperations proves an initialized SDK session cannot execute cached removed operations.
func TestDeletedServiceEvictsWarmOperations(t *testing.T) {
	s := &lifecycleSandboxStore{deleted: true}
	cache := NewLocalObjectCache(s)
	id, version, app := uuid.New().String(), uuid.New().String(), uuid.New().String()
	cache.sdkVersions[app] = map[string]string{id: version}
	cache.serviceMetadataCache[id+":"+version] = &fusedobject.ServiceMetadata{Name: "Deleted service"}
	cache.endpointMetadataCache[id+":"+version+":list"] = &fusedobject.Endpoint{Name: "list"}
	_, err := cache.GetEndpoint(context.Background(), app, id, "list")
	// Both the call and retained payload must disappear when the authoritative gate reports deletion.
	if !errors.Is(err, store.ErrServiceDeleted) || len(cache.serviceMetadataCache) != 0 || len(cache.endpointMetadataCache) != 0 {
		t.Fatalf("warm deletion bypass: %v", err)
	}
}

// TestLifecyclePurgeFailureDoesNotAdvanceCursor proves a transient compaction failure cannot lose the removed event.
func TestLifecyclePurgeFailureDoesNotAdvanceCursor(t *testing.T) {
	id := uuid.New()
	s := &lifecycleSandboxStore{applyErr: errors.New("database unavailable")}
	registry := &changelogPollerMockRegistryClient{entries: map[uuid.UUID][]models.ServiceChangelogEntry{id: {{ID: uuid.New(), ServiceID: id, ConfigType: "version", ChangelogType: "removed", CreatedAt: time.Now()}}}}
	err := pollServiceChangelogForService(context.Background(), s, &changelogPollerMockConfigStore{}, registry, "test", id, nil)
	// Cursor ownership must stay behind any uncommitted payload purge.
	if err == nil || s.cursorUpserts != 0 || s.applied != 1 {
		t.Fatalf("failed purge advanced cursor: err=%v upserts=%d attempts=%d", err, s.cursorUpserts, s.applied)
	}
	s.applyErr = nil
	// The next pass replays and commits the same removal before acknowledging it.
	if err := pollServiceChangelogForService(context.Background(), s, &changelogPollerMockConfigStore{}, registry, "test", id, nil); err != nil {
		t.Fatal(err)
	}
	if s.cursorUpserts != 1 || s.applied != 2 {
		t.Fatal("removal was not retried")
	}
}

// TestServiceScheduleNotifiesEveryActivatedVersion proves service-wide schedules work without a single version name or an existing app.
func TestServiceScheduleNotifiesEveryActivatedVersion(t *testing.T) {
	id := uuid.New()
	entry := models.ServiceChangelogEntry{ServiceID: id, ConfigType: "version", ChangelogType: "deprecated", Diff: []byte(`{"lifecycle":true,"delete_at":"2099-01-01T00:00:00Z"}`)}
	_, matched := matchedVersionConfigKeys([]store.WorkspaceService{{ServiceID: id, ServiceVersionID: uuid.New()}}, nil, entry)
	// A concrete date must reach consumers even when they have not created an app yet.
	if !matched || !strings.Contains(changelogNotificationMessage(entry, store.WorkspaceNotificationTypeRegistryVersionDeprecated, nil), "2099-01-01T00:00:00Z") {
		t.Fatal("scheduled deletion warning lost deadline")
	}
	entry.ChangelogType = "changed"
	entry.Diff = []byte(`{"lifecycle":true,"delete_at":null}`)
	// Cancellation must replace the misleading endpoint-change description.
	if !strings.Contains(changelogNotificationMessage(entry, store.WorkspaceNotificationTypeRegistryVersionChanged, nil), "cancelled") {
		t.Fatal("missing cancellation notice")
	}
}

// TestLifecycleBacklogUsesCancellationInsteadOfExpiredSchedule covers a reconnect after both the original deadline and its earlier cancellation.
func TestLifecycleBacklogUsesCancellationInsteadOfExpiredSchedule(t *testing.T) {
	now := time.Now().UTC()
	entries := []models.ServiceChangelogEntry{
		{ConfigType: "version", ChangelogType: "deprecated", CreatedAt: now.Add(-2 * time.Hour), Diff: []byte(`{"lifecycle":true,"delete_at":"2000-01-01T00:00:00Z"}`)},
		{ConfigType: "version", ChangelogType: "changed", CreatedAt: now.Add(-time.Hour), Diff: []byte(`{"lifecycle":true,"delete_at":null}`)},
	}
	latest, at := latestServiceLifecycleEntry(entries)
	// Only the terminal schedule state may be applied, regardless of receipt delivery order.
	if latest != &entries[1] || at != nil {
		t.Fatal("obsolete schedule won over cancellation")
	}
	latest, at = latestServiceLifecycleEntry([]models.ServiceChangelogEntry{entries[1], entries[0]})
	// Out-of-order transport must preserve timestamp authority as well.
	if latest.ChangelogType != "changed" || at != nil {
		t.Fatal("out-of-order replay restored deadline")
	}
}
