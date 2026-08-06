package accesscontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestCheckAllRequiresEveryUniqueRequirement(t *testing.T) {
	workspaceID := uuid.New()
	allowedServiceID := uuid.New()
	deniedBucketID := uuid.New()
	snapshot := mustSnapshot(t,
		Grant{Permission: PermissionAppCreate, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}},
		Grant{Permission: PermissionServiceConsume, Resource: ResourceRef{Type: ResourceService, ID: allowedServiceID}},
	)
	actor := Actor{Authorization: snapshot}
	authorizer := SnapshotAuthorizer{}

	requirements := []Requirement{
		{Permission: PermissionAppCreate, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}},
		{Permission: PermissionServiceConsume, Resource: ResourceRef{Type: ResourceService, ID: allowedServiceID}},
		{Permission: PermissionBucketUse, Resource: ResourceRef{Type: ResourceBucket, ID: deniedBucketID}},
		{Permission: PermissionBucketUse, Resource: ResourceRef{Type: ResourceBucket, ID: deniedBucketID}},
	}
	err := authorizer.CheckAll(context.Background(), actor, requirements...)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("CheckAll error = %v, want ErrPermissionDenied", err)
	}
	var denied *PermissionDeniedError
	if !errors.As(err, &denied) || len(denied.Missing) != 1 {
		t.Fatalf("CheckAll missing = %#v, want one deduplicated requirement", denied)
	}
}

func TestWorkspaceGrantAllowsResourceRequirements(t *testing.T) {
	workspaceID := uuid.New()
	snapshot := mustSnapshot(t, Grant{
		Permission: PermissionServiceRead,
		Resource:   ResourceRef{Type: ResourceWorkspace, ID: workspaceID},
	})
	requirement := Requirement{
		Permission: PermissionServiceRead,
		Resource:   ResourceRef{Type: ResourceService, ID: uuid.New()},
	}
	if err := (SnapshotAuthorizer{}).CheckAll(context.Background(), Actor{Authorization: snapshot}, requirement); err != nil {
		t.Fatalf("CheckAll: %v", err)
	}
}

func TestScopedGrantDoesNotAllowAnotherResource(t *testing.T) {
	allowedID := uuid.New()
	snapshot := mustSnapshot(t, Grant{
		Permission: PermissionBucketUse,
		Resource:   ResourceRef{Type: ResourceBucket, ID: allowedID},
	})
	authorizer := SnapshotAuthorizer{}

	allowed := Requirement{Permission: PermissionBucketUse, Resource: ResourceRef{Type: ResourceBucket, ID: allowedID}}
	if err := authorizer.CheckAll(context.Background(), Actor{Authorization: snapshot}, allowed); err != nil {
		t.Fatalf("allowed CheckAll: %v", err)
	}
	denied := Requirement{Permission: PermissionBucketUse, Resource: ResourceRef{Type: ResourceBucket, ID: uuid.New()}}
	if err := authorizer.CheckAll(context.Background(), Actor{Authorization: snapshot}, denied); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("denied CheckAll error = %v, want ErrPermissionDenied", err)
	}
}

func TestScopeDeduplicatesAndSortsResourceIDs(t *testing.T) {
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	snapshot := mustSnapshot(t,
		Grant{Permission: PermissionBucketRead, Resource: ResourceRef{Type: ResourceBucket, ID: second}},
		Grant{Permission: PermissionBucketRead, Resource: ResourceRef{Type: ResourceBucket, ID: first}},
		Grant{Permission: PermissionBucketRead, Resource: ResourceRef{Type: ResourceBucket, ID: second}},
	)
	scope, err := (SnapshotAuthorizer{}).Scope(context.Background(), Actor{Authorization: snapshot}, PermissionBucketRead, ResourceBucket)
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	if scope.All || len(scope.IDs) != 2 || scope.IDs[0] != first || scope.IDs[1] != second {
		t.Fatalf("Scope = %#v, want sorted unique IDs", scope)
	}
}

func TestWorkspaceScopeReportsAllWithoutResourceIDs(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	snapshot := mustSnapshot(t,
		Grant{Permission: PermissionBucketRead, Resource: ResourceRef{Type: ResourceBucket, ID: bucketID}},
		Grant{Permission: PermissionBucketRead, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}},
	)
	scope, err := (SnapshotAuthorizer{}).Scope(context.Background(), Actor{Authorization: snapshot}, PermissionBucketRead, ResourceBucket)
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	if !scope.All || len(scope.IDs) != 0 {
		t.Fatalf("Scope = %#v, want All without retained IDs", scope)
	}
}

func TestEffectiveGrantsProjectsWorkspaceAndScopedAccessInStableOrder(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	serviceID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	snapshot := mustSnapshot(t,
		Grant{Permission: PermissionServiceRead, Resource: ResourceRef{Type: ResourceService, ID: serviceID}},
		Grant{Permission: PermissionWorkspaceRead, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}},
		Grant{Permission: PermissionBucketUse, Resource: ResourceRef{Type: ResourceBucket, ID: bucketID}},
	)

	got := snapshot.EffectiveGrants(workspaceID)
	want := []Grant{
		{Permission: PermissionBucketUse, Resource: ResourceRef{Type: ResourceBucket, ID: bucketID}},
		{Permission: PermissionServiceRead, Resource: ResourceRef{Type: ResourceService, ID: serviceID}},
		{Permission: PermissionWorkspaceRead, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}},
	}
	if len(got) != len(want) {
		t.Fatalf("EffectiveGrants length = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("EffectiveGrants[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestSnapshotRejectsUnknownPermissionAndMissingResourceID(t *testing.T) {
	tests := []Grant{
		{Permission: "unknown", Resource: ResourceRef{Type: ResourceWorkspace, ID: uuid.New()}},
		{Permission: PermissionWorkspaceRead, Resource: ResourceRef{Type: ResourceWorkspace}},
	}
	for _, grant := range tests {
		if _, err := NewAuthorizationSnapshot(1, grant); !errors.Is(err, ErrInvalidRequirement) {
			t.Fatalf("NewAuthorizationSnapshot(%#v) error = %v, want ErrInvalidRequirement", grant, err)
		}
	}
}

func TestCheckAllWritesSafeOTELDecisionEvent(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	ctx, span := provider.Tracer("test").Start(context.Background(), "request")
	requirement := Requirement{Permission: PermissionAuditRead, Resource: ResourceRef{Type: ResourceWorkspace, ID: uuid.New()}}
	err := (SnapshotAuthorizer{}).CheckAll(ctx, Actor{}, requirement)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("CheckAll error = %v, want ErrPermissionDenied", err)
	}
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 || len(spans[0].Events) != 1 {
		t.Fatalf("exported spans/events = %d/%d, want 1/1", len(spans), len(spans[0].Events))
	}
	event := spans[0].Events[0]
	if event.Name != "engine.authorization.check" {
		t.Fatalf("event name = %q", event.Name)
	}
	for _, attr := range event.Attributes {
		if attr.Key == "subject_id" || attr.Key == "credential_hash" || attr.Key == "resource_id" {
			t.Fatalf("OTEL event contains sensitive/high-cardinality attribute %q", attr.Key)
		}
	}
}

func mustSnapshot(t *testing.T, grants ...Grant) AuthorizationSnapshot {
	t.Helper()
	snapshot, err := NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		t.Fatalf("NewAuthorizationSnapshot: %v", err)
	}
	return snapshot
}
