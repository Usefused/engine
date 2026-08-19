package accesscontrol

import (
	"context"
	"testing"
)

func TestMutationAuditEvidenceIsBoundedAndAccumulatesFacts(t *testing.T) {
	ctx := ContextWithMutationAuditEvidence(context.Background())
	MarkMutationAuditUnchanged(ctx)
	MarkMutationAuditRolledBack(ctx)
	MarkMutationAuditCancelled(ctx)

	evidence, ok := MutationAuditEvidenceFromContext(ctx)
	if !ok || !evidence.Unchanged || !evidence.RolledBack || !evidence.Cancelled {
		t.Fatalf("mutation audit evidence = %#v/%v", evidence, ok)
	}
}

// TestConnectBrandingAuditEvidenceRecordsChangedAndConvergedResults verifies
// branding metadata presence is independent from whether the change count is zero.
func TestConnectBrandingAuditEvidenceRecordsChangedAndConvergedResults(t *testing.T) {
	for _, test := range []struct {
		name    string
		changes ConnectBrandingAuditChanges
	}{
		{name: "changed", changes: ConnectBrandingAuditChanges{DisplayName: true, LogoURL: true, Count: 2}},
		{name: "converged", changes: ConnectBrandingAuditChanges{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := ContextWithMutationAuditEvidence(context.Background())
			MarkConnectBrandingAuditChanges(ctx, test.changes)
			evidence, ok := MutationAuditEvidenceFromContext(ctx)
			if !ok || !evidence.ConnectBrandingChanges.Present || evidence.ConnectBrandingChanges.Count != test.changes.Count {
				t.Fatalf("branding evidence = %#v, present=%v", evidence, ok)
			}
		})
	}
}

func TestMutationAuditMarkersIgnoreUninstrumentedContext(t *testing.T) {
	ctx := context.Background()
	MarkMutationAuditUnchanged(ctx)
	MarkMutationAuditRolledBack(ctx)
	MarkMutationAuditCancelled(ctx)
	MarkConnectBrandingAuditChanges(ctx, ConnectBrandingAuditChanges{DisplayName: true, Count: 1})
	if _, ok := MutationAuditEvidenceFromContext(ctx); ok {
		t.Fatal("uninstrumented context unexpectedly exposed mutation evidence")
	}
}
