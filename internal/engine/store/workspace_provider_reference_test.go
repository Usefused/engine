package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runProviderReferenceRepair executes the shipped transaction on one connection, including rollback on validation failure.
func runProviderReferenceRepair(t *testing.T, ctx context.Context, pool *pgxpool.Pool) error {
	t.Helper()
	script, err := os.ReadFile("../../../scripts/repair-workspace-service-references.sql")
	// Test the operator artifact itself so repair behavior cannot drift from a copied SQL fixture.
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pool.Acquire(ctx)
	// The script's explicit transaction must remain on one database session.
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	_, err = connection.Exec(ctx, string(script))
	// Expected rejection must release its locks without contaminating later test reads.
	if err != nil {
		_, _ = connection.Exec(ctx, "ROLLBACK")
	}
	return err
}

// seedProviderReferenceSnapshot pins provider evidence to the exact version already enabled by a test.
func seedProviderReferenceSnapshot(t *testing.T, ctx context.Context, repository *postgresStore, serviceID, versionID uuid.UUID, provider string) {
	t.Helper()
	snapshot := serviceContractBatchFixture(0)
	snapshot.ServiceID, snapshot.ServiceVersionID, snapshot.Version = serviceID, versionID, "v1"
	snapshot.ServiceMetadata.ID, snapshot.ServiceMetadata.ServiceVersionID = serviceID, versionID
	snapshot.ServiceMetadata.Provider = &models.ServiceProviderIdentity{Handle: provider}
	// Only admitted snapshot metadata may authorize the SQL repair's provider choice.
	if _, err := repository.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
}

// TestWorkspaceProviderReferencesAndRepair reproduces a consumed Plunk followed by another account's Plunk activation.
func TestWorkspaceProviderReferencesAndRepair(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	a, b, versionA, versionB, owner := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// Old Engine releases stored the consumed service without its namespace.
	if err := repository.AddWorkspaceServiceVersion(ctx, a, "plunk", "v1", versionA, "Plunk", owner); err != nil {
		t.Fatal(err)
	}
	seedProviderReferenceSnapshot(t, ctx, repository, a, versionA, "account-a")
	// The shipped SQL must repair the old row before owned-service recovery needs the same bare name.
	if err := runProviderReferenceRepair(t, ctx, pool); err != nil {
		t.Fatal(err)
	}
	// A second provider's identically named upload now activates without changing the first service's ID or pin.
	if err := repository.AddWorkspaceServiceVersion(ctx, b, "@account-b/plunk", "v1", versionB, "Plunk", owner); err != nil {
		t.Fatal(err)
	}
	refs, err := repository.ResolveWorkspaceServiceIDsByKeys(ctx, []string{"@account-a/plunk", "@account-b/plunk", "@wrong/plunk", "plunk"})
	// Qualified references select exactly their provider; ambiguous bare and wrong-provider references grant no identity.
	if err != nil || len(refs) != 2 || refs["@account-a/plunk"] != a || refs["@account-b/plunk"] != b {
		t.Fatalf("resolved=%v err=%v", refs, err)
	}
	rows, err := repository.ListWorkspaceServices(ctx, []string{"@account-a/plunk"})
	// Filtered listing must not mix same-named providers and must retain the pre-repair pin.
	if err != nil || len(rows) != 1 || rows[0].ServiceID != a || rows[0].ServiceVersionID != versionA {
		t.Fatalf("listed=%v err=%v", rows, err)
	}
	rows, count, err := repository.ListAuthorizedWorkspaceServicesPage(ctx, accesscontrol.AuthorizedScope{All: true}, []string{"@account-b/plunk"}, 10, 0)
	// Authorization-aware UI/CLI listing shares the same exact namespace boundary.
	if err != nil || count != 1 || len(rows) != 1 || rows[0].ServiceID != b {
		t.Fatalf("authorized=%v count=%d err=%v", rows, count, err)
	}
	_, err = repository.ResolveResourceReference(ctx, ResourceReferenceQuery{Kind: ReferenceService, Value: "plunk", AllowedAll: true})
	// An ambiguous human label must never authorize whichever service happens to sort first.
	if err == nil {
		t.Fatal("ambiguous access-control reference resolved")
	}
	// Reapplying the repair leaves already-qualified rows and all pins unchanged.
	if err := runProviderReferenceRepair(t, ctx, pool); err != nil {
		t.Fatal(err)
	}
}

// TestWorkspaceProviderRepairRejectsUnprovenIdentity verifies unsafe repairs roll back instead of assigning a guessed provider.
func TestWorkspaceProviderRepairRejectsUnprovenIdentity(t *testing.T) {
	for _, scenario := range []string{"missing provider", "conflicting providers", "claimed destination"} {
		// Each scenario uses a fresh schema so no previous rejected transaction can affect the result.
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel, pool, repository := accessControlTestRepository(t)
			defer cancel()
			defer pool.Close()
			serviceID, versionID, owner := uuid.New(), uuid.New(), uuid.New()
			// Every rejection starts with the same valid legacy membership and pin.
			if err := repository.AddWorkspaceServiceVersion(ctx, serviceID, "plunk", "v1", versionID, "Plunk", owner); err != nil {
				t.Fatal(err)
			}
			// Absence of evidence is tested separately from contradictory or occupied identity.
			if scenario != "missing provider" {
				seedProviderReferenceSnapshot(t, ctx, repository, serviceID, versionID, "account-a")
			}
			// Conflicting version metadata must not be resolved by choosing the first snapshot.
			if scenario == "conflicting providers" {
				otherVersion := uuid.New()
				// Enable a second exact version so both pieces of provider evidence belong to the current membership.
				if err := repository.EnableWorkspaceServiceVersion(ctx, serviceID, "v2", otherVersion, owner); err != nil {
					t.Fatal(err)
				}
				seedProviderReferenceSnapshot(t, ctx, repository, serviceID, otherVersion, "account-b")
			}
			// A destination bound to another immutable ID cannot be overwritten by the repair.
			if scenario == "claimed destination" {
				if err := repository.AddWorkspaceServiceVersion(ctx, uuid.New(), "@account-a/plunk", "v1", uuid.New(), "Other", owner); err != nil {
					t.Fatal(err)
				}
			}
			// Every unsafe case must fail without committing a partial rename.
			if err := runProviderReferenceRepair(t, ctx, pool); err == nil {
				t.Fatal("unsafe provider repair succeeded")
			}
			var slug string
			// Rollback preserves the original row for a reviewed Registry-backed repair.
			if err := pool.QueryRow(ctx, "SELECT service_slug FROM fused_workspace_services WHERE service_id=$1", serviceID).Scan(&slug); err != nil || slug != "plunk" {
				t.Fatalf("slug=%s err=%v", slug, err)
			}
		})
	}
}

// TestWorkspaceProviderWebhookReferences prevents ambiguous ingress routing while retaining legacy case-insensitive aliases.
func TestWorkspaceProviderWebhookReferences(t *testing.T) {
	ctx, cancel, pool, repository := accessControlTestRepository(t)
	defer cancel()
	defer pool.Close()
	owner := uuid.New()
	// Ingress joins the singleton workspace to obtain the callback account identity.
	if _, err := repository.BootstrapWorkspace(ctx, owner, "Provider reference test"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"@account-a/Plunk", "@account-b/plunk"} {
		id, versionID := uuid.New(), uuid.New()
		// Store mixed case deliberately to cover preexisting ingress compatibility.
		if err := repository.AddWorkspaceServiceVersion(ctx, id, ref, "v1", versionID, "Plunk", owner); err != nil {
			t.Fatal(err)
		}
		// Each service has the same label but a distinct immutable webhook target.
		if _, err := seedOwnedWorkspaceWebhooks(ctx, repository, []WorkspaceWebhook{{ServiceID: id, ServiceVersionID: versionID, Label: "prod", Slug: uuid.NewString(), OwningConfigKey: "webhook:prod"}}); err != nil {
			t.Fatal(err)
		}
		webhook, err := repository.GetWorkspaceWebhookByServiceAndLabel(ctx, ref, "prod")
		// Qualified lookup remains exact even when two providers expose the same slug.
		if err != nil || webhook.ServiceID != id {
			t.Fatalf("reference=%s webhook=%v err=%v", ref, webhook, err)
		}
	}
	for _, ref := range []string{"plunk", "@wrong/plunk"} {
		_, err := repository.GetWorkspaceWebhookByServiceAndLabel(ctx, ref, "prod")
		// Ambiguous bare aliases and explicitly wrong providers cannot pick an arbitrary webhook.
		if !errors.Is(err, ErrWorkspaceWebhookNotFound) {
			t.Fatalf("reference=%s err=%v", ref, err)
		}
	}
}
