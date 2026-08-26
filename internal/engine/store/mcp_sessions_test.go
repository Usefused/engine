package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMCPSessionPaginationContract bounds history and binds continuation to its exact app.
func TestMCPSessionPaginationContract(t *testing.T) {
	appID := uuid.New()
	items := []models.MCPSession{{ID: uuid.New(), StartedAt: time.Now().UTC()}, {ID: uuid.New(), StartedAt: time.Now().UTC()}}
	page := completeMCPSessionPage(appID, items, 1)
	// The extra SQL row proves continuation without becoming a duplicate visible row.
	if !page.HasMore || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatal("missing bounded continuation")
	}
	cursor, limit, err := validateMCPSessionPage(appID, page.NextCursor, 1)
	// The cursor keeps both sort keys so tied start times cannot drop rows.
	if err != nil || limit != 1 || cursor.ID != items[0].ID || !cursor.StartedAt.Equal(items[0].StartedAt) {
		t.Fatal("cursor did not round trip")
	}
	for _, test := range []struct {
		app   uuid.UUID
		after string
		first int
	}{{uuid.New(), page.NextCursor, 1}, {appID, "bad", 1}, {appID, "", -1}, {appID, "", 101}, {appID, strings.Repeat("a", 513), 1}} {
		// Invalid bounds and cross-app cursors fail before any store read.
		if _, _, err := validateMCPSessionPage(test.app, test.after, test.first); err == nil {
			t.Fatal("invalid pagination accepted")
		}
	}
}

// TestMCPSessionPageQueryScopesBeforePagination keeps account, app, and continuation predicates in SQL.
func TestMCPSessionPageQueryScopesBeforePagination(t *testing.T) {
	account, app := uuid.New(), uuid.New()
	cursor := &mcpSessionCursor{AppID: app, ID: uuid.New(), StartedAt: time.Now().UTC()}
	query, args := mcpSessionPageQuery(account, app, cursor, 25)
	for _, clause := range []string{"app.account_id = $1", "session.app_id = $2", "(session.started_at, session.id) < ($4::timestamptz, $5::uuid)", "ORDER BY session.started_at DESC, session.id DESC LIMIT $3"} {
		// No authorized scope may be implemented by post-query filtering.
		if !strings.Contains(query, clause) {
			t.Fatalf("missing SQL clause %q", clause)
		}
	}
	// The bounded sentinel is the only row beyond the requested page size.
	if len(args) != 5 || args[0] != account || args[1] != app || args[2] != 26 {
		t.Fatalf("unexpected SQL args %#v", args)
	}
}

// TestMCPSessionHistoryPostgres exercises lifecycle replay, tied cursors, and cross-account isolation in one owned schema.
func TestMCPSessionHistoryPostgres(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	seedMCPSessionHistoryApp(t, fixture)
	started := time.Now().UTC().Truncate(time.Microsecond)
	for index := 1; index <= 3; index++ {
		id := uuid.UUID{}
		id[15] = byte(index)
		session := models.MCPSession{ID: id, AppID: fixture.event.AppID, SessionID: id.String(), ProtocolVersion: "2025-06-18", StartedAt: started, LastActivityAt: started, ClientName: "Example Agent", ClientVersion: "1", InitialClientIP: "2001:db8::1"}
		// Synthetic rows share a timestamp to prove the UUID tie-breaker across page boundaries.
		if err := fixture.repository.UpsertMCPSession(fixture.ctx, &session); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}
	assertMCPSessionHistoryPages(t, fixture)
	assertMCPSessionReplayMetadata(t, fixture, started)
}

// seedMCPSessionHistoryApp creates only synthetic ownership rows inside the test's isolated schema.
func seedMCPSessionHistoryApp(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	team := seedAppOwnerTeam(t, fixture.ctx, fixture.pool)
	_, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_app_families (app_family_id, account_id, kind, canonical_name, display_name, owner_team_id) VALUES ($1,$2,'mcp',$3,'Session test',$4)`, fixture.event.AppFamilyID, fixture.event.AccountID, fixture.event.AppFamilyID.String(), team)
	// A valid ownership relation is required for the real tenant-scoped query.
	if err != nil {
		t.Fatalf("seed family: %v", err)
	}
	_, err = fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_apps (app_id,app_family_id,account_id,version,config_key,source_hash,status) VALUES ($1,$2,$3,'1.0.0',$4,'session-test','active')`, fixture.event.AppID, fixture.event.AppFamilyID, fixture.event.AccountID, "mcp:session-test:"+fixture.event.AppID.String())
	// Exact app identity prevents accidental family-wide session mixing.
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
}

// assertMCPSessionHistoryPages proves two rows per page, stable ordering, one query per page, and tenant denial.
func assertMCPSessionHistoryPages(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	tracer := &executionAnalyticsQueryTracer{}
	config := fixture.pool.Config()
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	// The tracing pool must address the same isolated schema as the fixture.
	if err != nil {
		t.Fatalf("tracing pool: %v", err)
	}
	defer pool.Close()
	repository := &postgresStore{db: pool}
	first, err := repository.ListMCPSessions(fixture.ctx, fixture.event.AccountID, fixture.event.AppID, "", 2)
	assertMCPSessionPage(t, first, err, 2, 3, true)
	second, err := repository.ListMCPSessions(fixture.ctx, fixture.event.AccountID, fixture.event.AppID, first.NextCursor, 2)
	assertMCPSessionPage(t, second, err, 1, 1, false)
	denied, err := repository.ListMCPSessions(fixture.ctx, uuid.New(), fixture.event.AppID, "", 2)
	// SQL ownership removes every row even when an attacker knows the exact app ID.
	if err != nil || len(denied.Items) != 0 || tracer.queries.Load() != 3 {
		t.Fatalf("tenant isolation/query count failed: %v/%d", err, tracer.queries.Load())
	}
}

// assertMCPSessionPage verifies each bounded SQL page without repeating cursor invariants.
func assertMCPSessionPage(t *testing.T, page MCPSessionPage, err error, count int, firstID byte, hasMore bool) {
	t.Helper()
	// The stable timestamp/UUID ordering must survive the exact page boundary.
	if err != nil || len(page.Items) != count || page.HasMore != hasMore || page.Items[0].ID[15] != firstID {
		t.Fatalf("session page = %#v/%v", page, err)
	}
}

// assertMCPSessionReplayMetadata ensures delayed starts cannot reopen ended sessions or replace their initial provenance.
func assertMCPSessionReplayMetadata(t *testing.T, fixture executionActivityFixture, started time.Time) {
	t.Helper()
	id := uuid.UUID{}
	id[15] = 3
	ended := started.Add(time.Minute)
	session := models.MCPSession{ID: id, AppID: fixture.event.AppID, SessionID: id.String(), ProtocolVersion: "2025-06-18", StartedAt: ended, LastActivityAt: ended, EndedAt: &ended, EndReason: "client_terminated", ClientName: "Replacement", InitialClientIP: "192.0.2.9"}
	// An end replay carries metadata but cannot rewrite the already recorded initial peer.
	if err := fixture.repository.UpsertMCPSession(fixture.ctx, &session); err != nil {
		t.Fatal(err)
	}
	session.StartedAt, session.LastActivityAt, session.EndedAt, session.EndReason = started, started, nil, ""
	// A late start must preserve termination and the latest activity timestamp.
	if err := fixture.repository.UpsertMCPSession(fixture.ctx, &session); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.repository.ListMCPSessions(context.Background(), fixture.event.AccountID, fixture.event.AppID, "", 1)
	// Ended sessions remain visible in the same retained history.
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("replayed page failed: %v", err)
	}
	got := page.Items[0]
	// Provenance is immutable while chronology advances monotonically.
	if got.ClientName != "Example Agent" || got.InitialClientIP != "2001:db8::1" || got.EndedAt == nil || !got.StartedAt.Equal(started) || !got.LastActivityAt.Equal(ended) {
		t.Fatalf("replay changed history: %#v", got)
	}
}

// TestMCPSessionInitializedMetadataPostgres proves SSE enrichment survives delayed starts and worker delivery reordering.
func TestMCPSessionInitializedMetadataPostgres(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	seedMCPSessionHistoryApp(t, fixture)
	started := time.Now().UTC().Truncate(time.Microsecond)
	ended := started.Add(time.Minute)
	session := models.MCPSession{ID: uuid.New(), AppID: fixture.event.AppID, SessionID: "synthetic-reordered", ProtocolVersion: "2025-06-18", StartedAt: ended, LastActivityAt: ended, EndedAt: &ended, EndReason: "client_terminated"}
	// Delivery may begin with termination after a worker restart; replay must still recover the true start.
	if err := fixture.repository.UpsertMCPSession(fixture.ctx, &session); err != nil {
		t.Fatal(err)
	}
	session.StartedAt, session.LastActivityAt, session.EndedAt, session.EndReason = started.Add(time.Second), started.Add(time.Second), nil, ""
	session.ClientName, session.ClientVersion, session.InitialClientIP = "Example Agent", "1", "192.0.2.2"
	// The initialized transition fills absent client fields on the same durable session row.
	if err := fixture.repository.UpsertMCPSession(fixture.ctx, &session); err != nil {
		t.Fatal(err)
	}
	session.StartedAt, session.LastActivityAt = started, started
	session.ClientName, session.ClientVersion, session.InitialClientIP = "", "", ""
	// A delayed metadata-free start cannot erase the richer initialization or reopen termination.
	if err := fixture.repository.UpsertMCPSession(fixture.ctx, &session); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.repository.ListMCPSessions(fixture.ctx, fixture.event.AccountID, fixture.event.AppID, "", 25)
	// All transitions merge into one row, never a second history stream.
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("reordered session page: %#v/%v", page, err)
	}
	got := page.Items[0]
	// Metadata and producer chronology must survive the independently ordered lifecycle deliveries.
	if got.ClientName != "Example Agent" || got.ClientVersion != "1" || got.InitialClientIP != "192.0.2.2" {
		t.Fatal("initialization metadata was lost")
	}
	assertMCPSessionChronology(t, got, started, ended)
}

// assertMCPSessionChronology centralizes the non-regression invariant for reordered lifecycle events.
func assertMCPSessionChronology(t *testing.T, session models.MCPSession, started, ended time.Time) {
	t.Helper()
	// Replay must preserve the earliest start, latest activity, and terminal state together.
	if !session.StartedAt.Equal(started) || !session.LastActivityAt.Equal(ended) || session.EndedAt == nil || !session.EndedAt.Equal(ended) || session.EndReason != "client_terminated" {
		t.Fatal("lifecycle chronology regressed")
	}
}
