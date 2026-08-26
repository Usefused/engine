package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestGenerationContractHashAdmission keeps legacy runtime compatibility separate from malformed new generation authority.
func TestGenerationContractHashAdmission(t *testing.T) {
	fixture := serviceContractBatchFixture(1)
	for _, hash := range []string{"sha256:" + strings.Repeat("a", 64), "", "sha256:" + strings.Repeat("A", 64), strings.Repeat("a", 64), "sha256:short"} {
		fixture.GenerationContractHash = hash
		_, _, err := prepareServiceContractSnapshot(fixture)
		wantValid := hash == "" || hash == "sha256:"+strings.Repeat("a", 64)
		// Empty legacy pins remain executable, but malformed supplied pins fail before writes.
		if (err == nil) != wantValid {
			t.Fatalf("hash=%q error=%v", hash, err)
		}
	}
}

type generationQueryTracer struct{ count atomic.Int64 }

// TraceQueryStart counts actual database statements rather than treating per-row batches as set-based evidence.
func (tracer *generationQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	tracer.count.Add(1)
	return ctx
}

// TraceQueryEnd satisfies pgx tracing without retaining SQL arguments or credentials.
func (*generationQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// openGenerationContractTestStore reuses the established integration database contract with a bounded test timeout.
func openGenerationContractTestStore(t *testing.T) (context.Context, *postgresStore, *generationQueryTracer) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	// Integration tests must never guess a developer's live database.
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, dsn)
	// Schema setup must finish before query counts are observed.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tracer := &generationQueryTracer{}
	config := pool.Config()
	config.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(ctx, config)
	// A dedicated pool keeps other package fixtures from contaminating the count.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	return ctx, NewPostgresStore(traced).(*postgresStore), tracer
}

// seedGenerationContractFixture creates only random test identities and cleans up their exact rows.
func seedGenerationContractFixture(t *testing.T, ctx context.Context, s *postgresStore, count int) ServiceContractSnapshot {
	t.Helper()
	snapshot := serviceContractBatchFixture(count)
	snapshot.GenerationContractHash = "sha256:" + strings.Repeat("a", 64)
	snapshot.Revision, snapshot.SourceHash = 7, "source-7"
	// Local activation is required before archived contracts can authorize a new plan.
	if err := s.AddWorkspaceServiceVersion(ctx, snapshot.ServiceID, "generation-test-"+snapshot.ServiceID.String(), snapshot.Version, snapshot.ServiceVersionID, "Generation fixture", uuid.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Cleanup is confined to this fixture's independently generated identities.
		if err := s.RemoveWorkspaceService(context.Background(), snapshot.ServiceID); err != nil {
			t.Error(err)
		}
		deleteServiceContractFixture(t, s.db, snapshot.ServiceVersionID)
	})
	// Runtime and generation metadata cross the same atomic persistence boundary.
	if _, err := s.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// TestPostgresGenerationContractsUseFixedQueries verifies exact identity and 1/100/1000-operation query-count invariance.
func TestPostgresGenerationContractsUseFixedQueries(t *testing.T) {
	ctx, s, tracer := openGenerationContractTestStore(t)
	for _, count := range []int{1, 100, 1000} {
		t.Run(fmt.Sprintf("operations_%d", count), func(t *testing.T) {
			snapshot := seedGenerationContractFixture(t, ctx, s, count)
			tracer.count.Store(0)
			assertGenerationContractReads(t, ctx, s, snapshot)
			// Binding, auth, and membership validation each own one SQL query regardless of cardinality.
			if got := tracer.count.Load(); got != 3 {
				t.Fatalf("queries=%d want=3", got)
			}
		})
	}
}

// assertGenerationContractReads verifies projected authority without loading raw schemas into the planning API.
func assertGenerationContractReads(t *testing.T, ctx context.Context, s *postgresStore, snapshot ServiceContractSnapshot) {
	t.Helper()
	bindings, err := s.ListGenerationContractBindings(ctx, []models.ServiceVersionRef{{ServiceID: snapshot.ServiceID, Version: snapshot.Version}}, true)
	// The compact pin must retain the exact Registry revision persisted at activation.
	if err != nil || len(bindings) != 1 || bindings[0].GenerationContractHash != snapshot.GenerationContractHash || bindings[0].Revision != 7 {
		t.Fatalf("bindings=%+v error=%v", bindings, err)
	}
	contracts, err := s.ListGenerationAuthContracts(ctx, []GenerationAuthSelection{{ServiceID: snapshot.ServiceID, Version: snapshot.Version, SelectAll: true}}, true)
	// Security projection must cover all selected operations without an N+1 fetch.
	if err != nil || len(contracts) != 1 || len(contracts[0].Operations) != len(snapshot.Endpoints) {
		t.Fatalf("contracts=%d error=%v", len(contracts), err)
	}
	selection := models.SDKSelection{ServiceID: snapshot.ServiceID, ServiceVersionID: snapshot.ServiceVersionID,
		OperationNames: []string{snapshot.Endpoints[0].Name}, WebhookNames: []string{snapshot.Webhooks[0].Name}}
	// Existing names must validate through their exact snapshot rather than another service's rows.
	if err := s.ValidateGenerationSelections(ctx, []models.SDKSelection{selection}, true); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresGenerationContractsRejectMissingNamesAndPins exercises SQL failure paths that mocks cannot prove.
func TestPostgresGenerationContractsRejectMissingNamesAndPins(t *testing.T) {
	ctx, s, _ := openGenerationContractTestStore(t)
	snapshot := seedGenerationContractFixture(t, ctx, s, 1)
	base := models.SDKSelection{ServiceID: snapshot.ServiceID, ServiceVersionID: snapshot.ServiceVersionID}
	for _, selection := range []models.SDKSelection{
		{ServiceID: base.ServiceID, ServiceVersionID: base.ServiceVersionID, OperationNames: []string{"missing"}},
		{ServiceID: base.ServiceID, ServiceVersionID: base.ServiceVersionID, WebhookNames: []string{"missing"}},
		{ServiceID: base.ServiceID, ServiceVersionID: base.ServiceVersionID, EndpointIDs: []uuid.UUID{uuid.New()}},
		{ServiceID: base.ServiceID, ServiceVersionID: uuid.New(), OperationNames: []string{snapshot.Endpoints[0].Name}},
	} {
		// A valid row elsewhere cannot satisfy a missing name or different immutable identity.
		if err := s.ValidateGenerationSelections(ctx, []models.SDKSelection{selection}, true); err == nil {
			t.Fatal("invalid selection admitted")
		}
	}
	snapshot.GenerationContractHash = ""
	// Preserve legacy execution state while removing generation authority in this isolated fixture.
	if _, err := s.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	_, err := s.ListGenerationContractBindings(ctx, []models.ServiceVersionRef{{ServiceID: snapshot.ServiceID, Version: snapshot.Version}}, true)
	// Empty pins require explicit refresh rather than a lookup fallback.
	if !errors.Is(err, ErrGenerationContractPinUnavailable) {
		t.Fatalf("error=%v", err)
	}
}

// TestPostgresGenerationServiceRefsKeepProviderAuthority proves local resolution never guesses an owner from a bare legacy slug.
func TestPostgresGenerationServiceRefsKeepProviderAuthority(t *testing.T) {
	ctx, s, tracer := openGenerationContractTestStore(t)
	suffix := uuid.NewString()
	qualified := "@verified/provider-" + suffix
	ambiguous := "@ambiguous/provider-" + suffix
	qualifiedID := seedGenerationServiceRef(t, ctx, s, qualified, qualified)
	snapshot := seedGenerationContractFixture(t, ctx, s, 1)
	snapshot.ServiceMetadata.Provider = &models.ServiceProviderIdentity{Name: "Verified provider", Handle: "verified"}
	// A normal refresh supplies the previously missing provider identity beside the generation pin.
	if _, err := s.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	bare := "generation-test-" + snapshot.ServiceID.String()
	seedGenerationServiceRef(t, ctx, s, "first-"+suffix, ambiguous)
	seedGenerationServiceRef(t, ctx, s, "second-"+suffix, ambiguous)
	wrong := "@unverified/" + bare
	qualifiedBare := "@verified/" + bare
	tracer.count.Store(0)
	resolved, err := s.ResolveGenerationServiceIDsByKeys(ctx, []string{qualified, bare, qualifiedBare, ambiguous, wrong})
	// Identity resolution remains one bounded SQL statement despite multiple providers and conflicts.
	if err != nil || tracer.count.Load() != 1 {
		t.Fatalf("queries=%d error=%v", tracer.count.Load(), err)
	}
	// Stored exact qualification and unqualified legacy identity remain usable.
	if resolved[qualified] != qualifiedID || resolved[bare] != snapshot.ServiceID || resolved[qualifiedBare] != snapshot.ServiceID {
		t.Fatalf("resolved=%+v", resolved)
	}
	// No first-row choice or provider-stripping fallback may grant an explicitly qualified reference.
	if _, exists := resolved[ambiguous]; exists {
		t.Fatal("ambiguous provider identity resolved")
	}
	// Wrong providers cannot borrow the matching bare service's retained contract.
	if _, exists := resolved[wrong]; exists {
		t.Fatal("unverified provider identity resolved")
	}
}

// TestPostgresGenerationServiceLegacyProviderRequiresRefresh keeps unknown provider identity distinct from an incorrect known provider.
func TestPostgresGenerationServiceLegacyProviderRequiresRefresh(t *testing.T) {
	ctx, s, _ := openGenerationContractTestStore(t)
	bare := "legacy-" + uuid.NewString()
	seedGenerationServiceRef(t, ctx, s, bare, bare)
	_, err := s.ResolveGenerationServiceIDsByKeys(ctx, []string{"@unverified/" + bare})
	// A refreshed snapshot can repair absent metadata; silently stripping the provider cannot.
	if !errors.Is(err, ErrServiceProviderIdentityUnavailable) {
		t.Fatalf("error=%v", err)
	}
}

// TestPostgresLocalMCPPlanningDoesNotRequireGenerationPin checks the shared SQL's local-only policy for older admitted snapshots.
func TestPostgresLocalMCPPlanningDoesNotRequireGenerationPin(t *testing.T) {
	ctx, s, _ := openGenerationContractTestStore(t)
	snapshot := seedGenerationContractFixture(t, ctx, s, 1)
	snapshot.GenerationContractHash = ""
	// This simulates a valid pre-archive runtime snapshot, not missing execution data.
	if _, err := s.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	refs := []models.ServiceVersionRef{{ServiceID: snapshot.ServiceID, Version: snapshot.Version}}
	bindings, err := s.ListGenerationContractBindings(ctx, refs, false)
	// The local hash supplies the MCP staleness fence without mislabelling it as a Registry object hash.
	if err != nil || len(bindings) != 1 || bindings[0].RuntimeContractHash == "" {
		t.Fatalf("bindings=%+v error=%v", bindings, err)
	}
	_, err = s.ListGenerationAuthContracts(ctx, []GenerationAuthSelection{{ServiceID: snapshot.ServiceID, Version: snapshot.Version, SelectAll: true}}, false)
	// Existing runtime auth metadata remains usable even when no package will be generated.
	if err != nil {
		t.Fatal(err)
	}
	err = s.ValidateGenerationSelections(ctx, []models.SDKSelection{{ServiceID: snapshot.ServiceID, ServiceVersionID: snapshot.ServiceVersionID, OperationNames: []string{snapshot.Endpoints[0].Name}}}, false)
	// Membership remains exact regardless of whether the adapter needs a Registry archive.
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ListGenerationContractBindings(ctx, refs, true)
	// The same snapshot must still fail closed when the caller actually requests SDK generation.
	if !errors.Is(err, ErrGenerationContractPinUnavailable) {
		t.Fatalf("SDK error=%v", err)
	}
}

// seedGenerationServiceRef creates one exact locally enabled identity without assuming Registry provider metadata exists.
func seedGenerationServiceRef(t *testing.T, ctx context.Context, s *postgresStore, slug, name string) uuid.UUID {
	t.Helper()
	serviceID := uuid.New()
	// This fixture uses the same activation writer as normal workspace setup.
	if err := s.AddWorkspaceServiceVersion(ctx, serviceID, slug, "v1", uuid.New(), name, uuid.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Only this fixture's random service ID is removed; sibling provider identities remain untouched.
		if err := s.RemoveWorkspaceService(context.Background(), serviceID); err != nil {
			t.Error(err)
		}
	})
	return serviceID
}
