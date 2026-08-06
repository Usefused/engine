package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// changelogFetchCall records one FetchServiceChangelogSince invocation so
// tests can assert both which service was asked about and what cursor value
// it was asked since.
type changelogFetchCall struct {
	serviceID uuid.UUID
	since     time.Time
}

// changelogPollerMockRegistryClient embeds the RegistryClient interface
// (nil) rather than implementing every method, the same pattern mockStore
// uses in manager_test.go -- only FetchServiceChangelogSince is exercised by
// the poller, so every other method panics if ever called, making an
// accidental extra call fail loudly instead of silently no-op'ing.
type changelogPollerMockRegistryClient struct {
	RegistryClient
	entries map[uuid.UUID][]models.ServiceChangelogEntry
	errs    map[uuid.UUID]error
	calls   []changelogFetchCall
}

func (m *changelogPollerMockRegistryClient) FetchServiceChangelogSince(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) ([]models.ServiceChangelogEntry, error) {
	m.calls = append(m.calls, changelogFetchCall{serviceID, since})
	if m.errs != nil {
		if err, ok := m.errs[serviceID]; ok {
			return nil, err
		}
	}
	return m.entries[serviceID], nil
}

// changelogPollerMockStore embeds store.Store (nil) for the same reason --
// only the methods the poller actually calls are overridden.
type changelogPollerMockStore struct {
	store.Store
	services      []store.WorkspaceService
	listErr       error
	cursors       map[uuid.UUID]time.Time
	cached        []models.ServiceChangelogEntry
	insertErr     error
	cursorUpserts int
}

func (m *changelogPollerMockStore) ListWorkspaceServices(ctx context.Context, names []string) ([]store.WorkspaceService, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.services, nil
}

func (m *changelogPollerMockStore) GetServiceChangelogCursor(ctx context.Context, serviceID uuid.UUID) (time.Time, error) {
	if t, ok := m.cursors[serviceID]; ok {
		return t, nil
	}
	// Mirrors fused_service_changelog_cursor's own DEFAULT 'epoch' (see
	// internal/engine/store's epochCursor) -- duplicated here as a literal
	// since that identifier belongs to a different package.
	return time.Unix(0, 0).UTC(), nil
}

func (m *changelogPollerMockStore) UpsertServiceChangelogCursor(ctx context.Context, serviceID uuid.UUID, lastCheckedAt time.Time) error {
	if m.cursors == nil {
		m.cursors = map[uuid.UUID]time.Time{}
	}
	m.cursors[serviceID] = lastCheckedAt
	m.cursorUpserts++
	return nil
}

func (m *changelogPollerMockStore) InsertServiceChangelogCacheEntries(ctx context.Context, entries []models.ServiceChangelogEntry) error {
	if m.insertErr != nil {
		return m.insertErr
	}
	m.cached = append(m.cached, entries...)
	return nil
}

func (m *changelogPollerMockStore) ListAppRuntimes(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]*store.AppRuntime, error) {
	return map[uuid.UUID]*store.AppRuntime{}, nil
}

// changelogPollerMockConfigStore embeds store.ConfigRepository (nil) --
// these tests only exercise the poller and cache insertion; the
// changelog-driven workspace notification matching path requires SDK config
// states which are not configured here, so ListConfigStates returning empty
// is enough for matchAndNotifyServiceChangelog to safely no-op without ever
// touching the nil-embedded remainder of the interface.
type changelogPollerMockConfigStore struct {
	store.ConfigRepository
}

func (m *changelogPollerMockConfigStore) ListConfigStates(ctx context.Context, configType store.ConfigType) ([]store.ConfigState, error) {
	return nil, nil
}

// TestPollServiceChangelogForService_AdvancesCursorToMaxReturned is the
// core correctness rule from plans/plan-service-changelog.md: the cursor
// must advance to the max registry_created_at among the rows *returned*,
// not to wall-clock time, and out-of-order rows must still resolve to the
// true max, not just the last element.
func TestPollServiceChangelogForService_AdvancesCursorToMaxReturned(t *testing.T) {
	serviceID := uuid.New()
	older := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)

	registryClient := &changelogPollerMockRegistryClient{
		entries: map[uuid.UUID][]models.ServiceChangelogEntry{
			serviceID: {
				{ID: uuid.New(), ServiceID: serviceID, CreatedAt: newer},
				{ID: uuid.New(), ServiceID: serviceID, CreatedAt: older},
			},
		},
	}
	engineStore := &changelogPollerMockStore{}
	configStore := &changelogPollerMockConfigStore{}

	if err := pollServiceChangelogForService(context.Background(), engineStore, configStore, registryClient, "key", serviceID, nil); err != nil {
		t.Fatalf("pollServiceChangelogForService() error = %v", err)
	}

	if len(engineStore.cached) != 2 {
		t.Fatalf("expected 2 cached entries, got %d", len(engineStore.cached))
	}
	got := engineStore.cursors[serviceID]
	if !got.Equal(newer) {
		t.Fatalf("expected cursor advanced to max returned timestamp %v, got %v", newer, got)
	}
}

// TestPollServiceChangelogForService_NoRowsLeavesCursorUntouched: an empty
// response must not write a cursor row at all (see the plan doc: "No rows
// back: cursor untouched, no write at all").
func TestPollServiceChangelogForService_NoRowsLeavesCursorUntouched(t *testing.T) {
	serviceID := uuid.New()
	registryClient := &changelogPollerMockRegistryClient{}
	engineStore := &changelogPollerMockStore{}
	configStore := &changelogPollerMockConfigStore{}

	if err := pollServiceChangelogForService(context.Background(), engineStore, configStore, registryClient, "key", serviceID, nil); err != nil {
		t.Fatalf("pollServiceChangelogForService() error = %v", err)
	}

	if engineStore.cursorUpserts != 0 {
		t.Fatalf("expected no cursor write for an empty poll, got %d", engineStore.cursorUpserts)
	}
	if len(engineStore.cached) != 0 {
		t.Fatalf("expected no cache writes for an empty poll, got %d", len(engineStore.cached))
	}
}

// TestPollServiceChangelogForService_UsesExistingCursorAsSince verifies the
// poller reads the persisted cursor and passes it straight through as the
// Registry call's `since`, rather than always starting from the epoch.
func TestPollServiceChangelogForService_UsesExistingCursorAsSince(t *testing.T) {
	serviceID := uuid.New()
	lastChecked := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	registryClient := &changelogPollerMockRegistryClient{}
	engineStore := &changelogPollerMockStore{cursors: map[uuid.UUID]time.Time{serviceID: lastChecked}}
	configStore := &changelogPollerMockConfigStore{}

	if err := pollServiceChangelogForService(context.Background(), engineStore, configStore, registryClient, "key", serviceID, nil); err != nil {
		t.Fatalf("pollServiceChangelogForService() error = %v", err)
	}

	if len(registryClient.calls) != 1 || !registryClient.calls[0].since.Equal(lastChecked) {
		t.Fatalf("expected Registry called with since=%v, got %+v", lastChecked, registryClient.calls)
	}
}

// TestPollServiceChangelogs_OneServiceFailureDoesNotStopOthers is the
// best-effort tolerance rule: one service erroring must not prevent every
// other service in the same tick from being polled.
func TestPollServiceChangelogs_OneServiceFailureDoesNotStopOthers(t *testing.T) {
	failing := uuid.New()
	healthy := uuid.New()
	newRow := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)

	registryClient := &changelogPollerMockRegistryClient{
		entries: map[uuid.UUID][]models.ServiceChangelogEntry{
			healthy: {{ID: uuid.New(), ServiceID: healthy, CreatedAt: newRow}},
		},
		errs: map[uuid.UUID]error{
			failing: errors.New("registry unreachable"),
		},
	}
	engineStore := &changelogPollerMockStore{
		services: []store.WorkspaceService{{ServiceID: failing}, {ServiceID: healthy}},
	}
	configStore := &changelogPollerMockConfigStore{}

	// pollServiceChangelogs only logs failures (it has no return value to
	// assert on), so the test proves the tolerance behavior indirectly: the
	// healthy service's cursor must still have advanced despite the other
	// service's error.
	pollServiceChangelogs(context.Background(), engineStore, configStore, registryClient, "key")

	if got := engineStore.cursors[healthy]; !got.Equal(newRow) {
		t.Fatalf("expected healthy service's cursor to advance despite the other service's failure, got %v", got)
	}
	if _, ok := engineStore.cursors[failing]; ok {
		t.Fatalf("expected no cursor write for the failing service")
	}
}

// TestMaxRegistryCreatedAt is a pure-function check for the helper the
// cursor-advance rule depends on.
func TestMaxRegistryCreatedAt(t *testing.T) {
	a := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	got := maxRegistryCreatedAt([]models.ServiceChangelogEntry{{CreatedAt: a}, {CreatedAt: b}, {CreatedAt: c}})
	if !got.Equal(b) {
		t.Fatalf("expected max = %v, got %v", b, got)
	}

	if got := maxRegistryCreatedAt(nil); !got.IsZero() {
		t.Fatalf("expected zero value for no entries, got %v", got)
	}
}

// TestServiceChangelogPollIntervalFromEnv is a pure-logic check (no DB, no
// ticker) for the FUSED_CHANGELOG_POLL_INTERVAL override: unset, empty, and
// invalid values must all fall back to the default rather than ever
// returning a zero or negative duration that would spin a ticker.
func TestServiceChangelogPollIntervalFromEnv(t *testing.T) {
	t.Run("unset falls back to default", func(t *testing.T) {
		t.Setenv(serviceChangelogPollIntervalEnvVar, "")
		if got := serviceChangelogPollIntervalFromEnv(); got != serviceChangelogPollInterval {
			t.Fatalf("expected default %v, got %v", serviceChangelogPollInterval, got)
		}
	})

	t.Run("valid override is honored", func(t *testing.T) {
		t.Setenv(serviceChangelogPollIntervalEnvVar, "5s")
		if got := serviceChangelogPollIntervalFromEnv(); got != 5*time.Second {
			t.Fatalf("expected 5s, got %v", got)
		}
	})

	t.Run("unparseable value falls back to default", func(t *testing.T) {
		t.Setenv(serviceChangelogPollIntervalEnvVar, "not-a-duration")
		if got := serviceChangelogPollIntervalFromEnv(); got != serviceChangelogPollInterval {
			t.Fatalf("expected default on parse error, got %v", got)
		}
	})

	t.Run("non-positive value falls back to default", func(t *testing.T) {
		t.Setenv(serviceChangelogPollIntervalEnvVar, "0s")
		if got := serviceChangelogPollIntervalFromEnv(); got != serviceChangelogPollInterval {
			t.Fatalf("expected default for a non-positive duration, got %v", got)
		}
		t.Setenv(serviceChangelogPollIntervalEnvVar, "-5s")
		if got := serviceChangelogPollIntervalFromEnv(); got != serviceChangelogPollInterval {
			t.Fatalf("expected default for a negative duration, got %v", got)
		}
	})
}
