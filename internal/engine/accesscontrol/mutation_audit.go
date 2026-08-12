package accesscontrol

import (
	"context"
	"sync/atomic"
)

type mutationAuditEvidenceKey struct{}

type mutationAuditEvidence struct {
	unchanged  atomic.Bool
	rolledBack atomic.Bool
	cancelled  atomic.Bool
}

type MutationAuditEvidence struct {
	Unchanged  bool
	RolledBack bool
	Cancelled  bool
}

// ContextWithMutationAuditEvidence gives a control mutation bounded boolean
// result markers. The fixed shape prevents handlers from smuggling request or
// response material into durable audit metadata.
func ContextWithMutationAuditEvidence(ctx context.Context) context.Context {
	return context.WithValue(ctx, mutationAuditEvidenceKey{}, &mutationAuditEvidence{})
}

func MarkMutationAuditUnchanged(ctx context.Context) {
	if evidence, ok := mutationAuditEvidenceFromContext(ctx); ok {
		evidence.unchanged.Store(true)
	}
}

// MarkMutationAuditRolledBack is called only after a transaction has actually
// rolled back. Keeping this as bounded context evidence avoids inferring
// transaction state from HTTP errors or response bodies.
func MarkMutationAuditRolledBack(ctx context.Context) {
	if evidence, ok := mutationAuditEvidenceFromContext(ctx); ok {
		evidence.rolledBack.Store(true)
	}
}

// MarkMutationAuditCancelled records a known cancellation at the mutation
// boundary without retaining the cancellation error or other request data.
func MarkMutationAuditCancelled(ctx context.Context) {
	if evidence, ok := mutationAuditEvidenceFromContext(ctx); ok {
		evidence.cancelled.Store(true)
	}
}

func MutationAuditEvidenceFromContext(ctx context.Context) (MutationAuditEvidence, bool) {
	evidence, ok := mutationAuditEvidenceFromContext(ctx)
	if !ok {
		return MutationAuditEvidence{}, false
	}
	return MutationAuditEvidence{
		Unchanged: evidence.unchanged.Load(), RolledBack: evidence.rolledBack.Load(), Cancelled: evidence.cancelled.Load(),
	}, true
}

func mutationAuditEvidenceFromContext(ctx context.Context) (*mutationAuditEvidence, bool) {
	evidence, ok := ctx.Value(mutationAuditEvidenceKey{}).(*mutationAuditEvidence)
	return evidence, ok && evidence != nil
}
