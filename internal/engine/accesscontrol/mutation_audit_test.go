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

func TestMutationAuditMarkersIgnoreUninstrumentedContext(t *testing.T) {
	ctx := context.Background()
	MarkMutationAuditUnchanged(ctx)
	MarkMutationAuditRolledBack(ctx)
	MarkMutationAuditCancelled(ctx)
	if _, ok := MutationAuditEvidenceFromContext(ctx); ok {
		t.Fatal("uninstrumented context unexpectedly exposed mutation evidence")
	}
}
