package sandbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// serviceChangelogPollInterval matches the general cadence of this
// codebase's periodic workers (see plans/plan-service-changelog.md,
// "## Phase 2"). A plain, easily-adjusted constant, not a hard requirement.
const serviceChangelogPollInterval = 30 * time.Second

// serviceChangelogPollIntervalEnvVar overrides serviceChangelogPollInterval
// when set to a valid time.ParseDuration string (e.g. "5s"). Exists so
// live-stack e2e/smoke tests (see changelog-notification-e2e-test.sh) don't
// have to wait for the normal polling cadence to observe a tick; unset in
// production, so normal deployments get the default unchanged.
const serviceChangelogPollIntervalEnvVar = "FUSED_CHANGELOG_POLL_INTERVAL"

// serviceChangelogPollIntervalFromEnv resolves the effective interval once
// per StartServiceChangelogPoller call: an invalid or unset env var silently
// falls back to the default rather than failing startup, since this knob is
// a testing convenience, not a piece of required configuration.
func serviceChangelogPollIntervalFromEnv() time.Duration {
	raw := os.Getenv(serviceChangelogPollIntervalEnvVar)
	if raw == "" {
		return serviceChangelogPollInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("invalid "+serviceChangelogPollIntervalEnvVar+", using default",
			slog.String("value", raw), slog.Duration("default", serviceChangelogPollInterval))
		return serviceChangelogPollInterval
	}
	return d
}

// StartServiceChangelogPoller runs the Engine-side capture half of Phase 2
// (see plans/plan-service-changelog.md): a ticker worker that, per active
// service, reads that service's own poll cursor, asks Registry for anything
// new since then, caches the results locally, then matches each freshly
// cached row against this workspace's actual usage, notifying only when
// something actually affects it. configStore was added to create workspace
// notifications; engineStore/registryClient/apiKey are unchanged from
// Phase 2.
func StartServiceChangelogPoller(ctx context.Context, engineStore store.Store, configStore store.ConfigRepository, registryClient RegistryClient, apiKey string) {
	// A single loop avoids concurrent cursor advances; the local runtime gate independently enforces received deadlines.
	go func() {
		pollTicker := time.NewTicker(serviceChangelogPollIntervalFromEnv())
		defer pollTicker.Stop()
		pollServiceChangelogs(ctx, engineStore, configStore, registryClient, apiKey)
		for {
			// Shutdown cancels the only reconciliation loop, including future ticks.
			select {
			case <-ctx.Done():
				return
			case <-pollTicker.C:
				pollServiceChangelogs(ctx, engineStore, configStore, registryClient, apiKey)
			}
		}
	}()
}

// pollServiceChangelogs runs one tick: every activated service gets its own
// round trip (one cursor read, one Registry call, one cache write, one
// cursor advance) -- see the plan doc's explicit "one row per service, one
// round trip per service per tick" decision. A single service's failure is
// logged and skipped, never aborts the rest of the tick.
//
// ListWorkspaceServices returns one row per (service, version) -- a
// workspace with two activated versions of the same service gets two rows
// sharing a ServiceID. serviceChangelogSince is scoped by service, not
// version, so this groups by ServiceID first: one Registry round trip per
// service regardless of how many of its versions are activated, and the
// full version-row group is handed to the matcher so it can resolve a
// changelog entry's version *name* to the specific ServiceVersionID the
// usage index is keyed by.
func pollServiceChangelogs(ctx context.Context, engineStore store.Store, configStore store.ConfigRepository, registryClient RegistryClient, apiKey string) {
	services, err := engineStore.ListWorkspaceServices(ctx, nil)
	if err != nil {
		slog.ErrorContext(ctx, "Service changelog poller failed to list workspace services", slog.Any("error", err))
		return
	}

	versionsByService := make(map[uuid.UUID][]store.WorkspaceService)
	for _, service := range services {
		versionsByService[service.ServiceID] = append(versionsByService[service.ServiceID], service)
	}

	for serviceID, versions := range versionsByService {
		if err := pollServiceChangelogForService(ctx, engineStore, configStore, registryClient, apiKey, serviceID, versions); err != nil {
			errStr := err.Error()
			// A 404 means the configured Registry doesn't know about this service
			// (e.g. a support/debug endpoint with a different dataset).
			// Treat it as a transient, low-severity event rather than an ERROR.
			if strings.Contains(errStr, "status 404") || strings.Contains(errStr, "404 page not found") {
				slog.WarnContext(ctx, "Service changelog poller: service not found on registry (skipping)",
					slog.String("service_id", serviceID.String()))
			} else {
				slog.ErrorContext(ctx, "Service changelog poller failed for service",
					slog.String("service_id", serviceID.String()), slog.Any("error", err))
			}
		}
	}
}

// pollServiceChangelogForService is the one-round-trip unit of work for a
// single service: read cursor, fetch, cache, match+notify, advance. The
// cursor only advances when rows actually came back, and always to the max
// registry_created_at among them -- never wall-clock time -- so a row whose
// async insert on the Registry side commits slightly late can never fall
// permanently behind (see the plan doc's race explanation).
//
// Matching runs after the cache insert but before the cursor advance,
// using the entries already in memory (no re-read). If the process
// crashes before the cursor advances, the next tick re-fetches and
// re-matches the same rows -- harmless, since notification creation is
// idempotent on registry_changelog_id (see config_repository.go's
// workspaceNotificationDedupeMatchSQL).
func pollServiceChangelogForService(ctx context.Context, engineStore store.Store, configStore store.ConfigRepository, registryClient RegistryClient, apiKey string, serviceID uuid.UUID, versions []store.WorkspaceService) error {
	lastCheckedAt, err := engineStore.GetServiceChangelogCursor(ctx, serviceID)
	if err != nil {
		return err
	}

	entries, err := registryClient.FetchServiceChangelogSince(ctx, serviceID, lastCheckedAt, apiKey)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	if err := engineStore.InsertServiceChangelogCacheEntries(ctx, entries); err != nil {
		return err
	}

	matchAndNotifyServiceChangelog(ctx, engineStore, configStore, versions, entries)
	// Persist lifecycle receipts before advancing the cursor so a failed purge is retried.
	if err := applyServiceLifecycleEntries(ctx, engineStore, serviceID, versions, entries); err != nil {
		return err
	}

	return engineStore.UpsertServiceChangelogCursor(ctx, serviceID, maxRegistryCreatedAt(entries))
}

// maxRegistryCreatedAt returns the latest CreatedAt among entries. Registry
// orders serviceChangelogSince by created_at ASC, so this is normally just
// the last element, but the max is computed explicitly rather than assumed
// so the cursor-advance rule holds even if that ordering guarantee ever
// changes upstream.
func maxRegistryCreatedAt(entries []models.ServiceChangelogEntry) time.Time {
	var max time.Time
	for _, entry := range entries {
		if entry.CreatedAt.After(max) {
			max = entry.CreatedAt
		}
	}
	return max
}

// applyServiceLifecycleEntries preserves the existing notification path while enforcing service-wide removal and scheduled deadlines.
func applyServiceLifecycleEntries(ctx context.Context, engineStore store.Store, id uuid.UUID, versions []store.WorkspaceService, entries []models.ServiceChangelogEntry) error {
	lifecycle, ok := engineStore.(store.ServiceLifecycleStore)
	// Lightweight test adapters can exercise notifications independently of durable lifecycle storage.
	if !ok {
		return nil
	}
	name := ""
	// Workspace identity remains available here even when Registry already compacted its definition.
	if len(versions) > 0 {
		name = versions[0].ServiceName
	}
	latest, latestDeadline := latestServiceLifecycleEntry(entries)
	// No lifecycle event means this page of changelog needs only the ordinary notification path.
	if latest == nil {
		return nil
	}
	return lifecycle.ApplyServiceLifecycle(ctx, id, name, latestDeadline, latest.ChangelogType == "removed", latest.CreatedAt)
}

// latestServiceLifecycleEntry coalesces an offline backlog before any obsolete schedule can affect stored definitions.
func latestServiceLifecycleEntry(entries []models.ServiceChangelogEntry) (*models.ServiceChangelogEntry, *time.Time) {
	// A reconnect can deliver a schedule and its cancellation together; apply only the latest state before considering compaction.
	var latest *models.ServiceChangelogEntry
	var latestDeadline *time.Time
	for index := range entries {
		entry := &entries[index]
		// A version-only change has no authority over the entire service.
		if entry.Version != nil || entry.ConfigType != "version" {
			continue
		}
		var diff struct {
			Lifecycle bool       `json:"lifecycle"`
			DeleteAt  *time.Time `json:"delete_at"`
		}
		_ = json.Unmarshal(entry.Diff, &diff)
		// Ignore ordinary contract changes and advisory deprecations without a deletion schedule.
		if entry.ChangelogType != "removed" && !diff.Lifecycle {
			continue
		}
		// Timestamp ordering also protects reconnects whose transport did not preserve row order.
		if latest == nil || !entry.CreatedAt.Before(latest.CreatedAt) {
			latest = entry
			latestDeadline = diff.DeleteAt
		}
	}
	return latest, latestDeadline
}
