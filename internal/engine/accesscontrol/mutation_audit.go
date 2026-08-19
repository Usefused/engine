package accesscontrol

import (
	"context"
	"sync/atomic"
)

type mutationAuditEvidenceKey struct{}

type mutationAuditEvidence struct {
	unchanged                   atomic.Bool
	rolledBack                  atomic.Bool
	cancelled                   atomic.Bool
	connectBrandingDisplayName  atomic.Bool
	connectBrandingLogoURL      atomic.Bool
	connectBrandingPrimaryColor atomic.Bool
	connectBrandingSupportURL   atomic.Bool
	connectBrandingPrivacyURL   atomic.Bool
	connectBrandingChangeCount  atomic.Int64
	connectBrandingRecorded     atomic.Bool
}

type MutationAuditEvidence struct {
	Unchanged              bool
	RolledBack             bool
	Cancelled              bool
	ConnectBrandingChanges ConnectBrandingAuditChanges
}

// ConnectBrandingAuditChanges is the fixed, value-free branding mutation
// evidence admitted to the durable control audit.
type ConnectBrandingAuditChanges struct {
	Present      bool
	DisplayName  bool
	LogoURL      bool
	PrimaryColor bool
	SupportURL   bool
	PrivacyURL   bool
	Count        int
}

// ContextWithMutationAuditEvidence gives a control mutation bounded boolean
// result markers. The fixed shape prevents handlers from smuggling request or
// response material into durable audit metadata.
func ContextWithMutationAuditEvidence(ctx context.Context) context.Context {
	return context.WithValue(ctx, mutationAuditEvidenceKey{}, &mutationAuditEvidence{})
}

// MarkMutationAuditUnchanged identifies a converged mutation without copying
// request or response values into durable audit state.
func MarkMutationAuditUnchanged(ctx context.Context) {
	if evidence, ok := mutationAuditEvidenceFromContext(ctx); ok {
		// The middleware-owned container accepts only this fixed convergence fact.
		evidence.unchanged.Store(true)
	}
}

// MarkConnectBrandingAuditChanges records only fixed boolean/count facts so
// the control audit can distinguish a visual update without storing its values.
func MarkConnectBrandingAuditChanges(ctx context.Context, changes ConnectBrandingAuditChanges) {
	if evidence, ok := mutationAuditEvidenceFromContext(ctx); ok {
		// Presence distinguishes a zero-change branding PUT from unrelated mutations.
		evidence.connectBrandingRecorded.Store(true)
		evidence.connectBrandingDisplayName.Store(changes.DisplayName)
		evidence.connectBrandingLogoURL.Store(changes.LogoURL)
		evidence.connectBrandingPrimaryColor.Store(changes.PrimaryColor)
		evidence.connectBrandingSupportURL.Store(changes.SupportURL)
		evidence.connectBrandingPrivacyURL.Store(changes.PrivacyURL)
		evidence.connectBrandingChangeCount.Store(int64(changes.Count))
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

// MutationAuditEvidenceFromContext returns an immutable bounded snapshot for
// final control-audit projection after the handler completes.
func MutationAuditEvidenceFromContext(ctx context.Context) (MutationAuditEvidence, bool) {
	evidence, ok := mutationAuditEvidenceFromContext(ctx)
	if !ok {
		// Callers without the middleware-owned container cannot invent evidence.
		return MutationAuditEvidence{}, false
	}
	return MutationAuditEvidence{
		Unchanged: evidence.unchanged.Load(), RolledBack: evidence.rolledBack.Load(), Cancelled: evidence.cancelled.Load(),
		ConnectBrandingChanges: ConnectBrandingAuditChanges{
			Present:     evidence.connectBrandingRecorded.Load(),
			DisplayName: evidence.connectBrandingDisplayName.Load(), LogoURL: evidence.connectBrandingLogoURL.Load(),
			PrimaryColor: evidence.connectBrandingPrimaryColor.Load(), SupportURL: evidence.connectBrandingSupportURL.Load(),
			PrivacyURL: evidence.connectBrandingPrivacyURL.Load(), Count: int(evidence.connectBrandingChangeCount.Load()),
		},
	}, true
}

// mutationAuditEvidenceFromContext resolves only the private context container
// installed at the control mutation boundary.
func mutationAuditEvidenceFromContext(ctx context.Context) (*mutationAuditEvidence, bool) {
	evidence, ok := ctx.Value(mutationAuditEvidenceKey{}).(*mutationAuditEvidence)
	return evidence, ok && evidence != nil
}
