// Package webhookstream fences long-lived SDK webhook receivers against
// runtime-token revocation and exact app-version invalidation.
package webhookstream

import (
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// CancellationReason describes the bounded Engine-owned reason that ended a
// local receiver registration without exposing credential material.
type CancellationReason uint32

const (
	CancellationReasonNone CancellationReason = iota
	CancellationReasonUnregistered
	CancellationReasonTokenInvalidated
	CancellationReasonAppInvalidated
	CancellationReasonRejected
)

// Registry owns local receiver cancellation and generation fences. Its zero
// value is ready for use so startup wiring cannot accidentally omit map setup.
type Registry struct {
	mu sync.Mutex

	nextID               uint64
	tokens               map[uuid.UUID]uint64
	apps                 map[uuid.UUID]uint64
	registrations        map[uint64]*Registration
	registrationsByToken map[uuid.UUID]map[uint64]struct{}
	registrationsByApp   map[uuid.UUID]map[uint64]struct{}
}

// Registration is a generation-fenced claim for one token and one exact app
// version. Callers revalidate the runtime identity before Confirm activates it.
type Registration struct {
	registry        *Registry
	id              uint64
	tokenID         uuid.UUID
	appID           uuid.UUID
	tokenGeneration uint64
	appGeneration   uint64
	done            chan struct{}
	reason          atomic.Uint32
	confirmed       bool
}

// NewRegistry constructs an empty receiver registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register captures the current token and exact-app generations before the
// caller repeats authorization. Generation state remains bounded because the
// authoritative revalidation, rather than an in-memory tombstone, decides
// whether a revoked token may proceed.
func (registry *Registry) Register(tokenID, appID uuid.UUID) (*Registration, bool) {
	// Missing or unusable identity cannot acquire a workspace-wide receiver.
	if registry == nil || tokenID == uuid.Nil || appID == uuid.Nil {
		return rejectedRegistration(), false
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.initializeLocked()

	registry.nextID++
	registration := &Registration{
		registry: registry, id: registry.nextID, tokenID: tokenID, appID: appID,
		tokenGeneration: registry.tokens[tokenID], appGeneration: registry.apps[appID], done: make(chan struct{}),
	}
	registry.registrations[registration.id] = registration
	registry.addIndexLocked(registry.registrationsByToken, tokenID, registration.id)
	registry.addIndexLocked(registry.registrationsByApp, appID, registration.id)
	return registration, true
}

// Confirm activates a registration only when reauthorization completed in the
// same token and app generations captured by Register.
func (registration *Registration) Confirm() bool {
	// A rejected or already-detached registration cannot become active later.
	if registration == nil || registration.registry == nil {
		return false
	}

	registry := registration.registry
	registry.mu.Lock()
	defer registry.mu.Unlock()
	// Pointer equality prevents a stale handle from confirming a recycled
	// numeric registration identity after cleanup.
	if registry.registrations[registration.id] != registration {
		return false
	}
	// Reauthorization is stale when either invalidation generation advanced.
	if !registry.matchesCurrentGenerationLocked(registration) {
		registry.detachLocked(registration, cancellationReasonForStaleLocked(registry, registration))
		return false
	}
	registration.confirmed = true
	return true
}

// IsCurrent reports whether Confirm succeeded and neither runtime identity has
// been invalidated since that confirmation.
func (registration *Registration) IsCurrent() bool {
	// Rejected registrations carry no registry and always remain unusable.
	if registration == nil || registration.registry == nil {
		return false
	}

	registry := registration.registry
	registry.mu.Lock()
	defer registry.mu.Unlock()
	// Both membership and the generation fence are required because unregister
	// removes the entry without advancing an application generation.
	return registration.confirmed &&
		registry.registrations[registration.id] == registration &&
		registry.matchesCurrentGenerationLocked(registration)
}

// Done closes when the registration is invalidated, rejected, or explicitly
// unregistered, allowing the gRPC owner to stop the existing stream promptly.
func (registration *Registration) Done() <-chan struct{} {
	// A nil handle is already unusable and therefore returns a closed channel.
	if registration == nil {
		return closedDoneChannel()
	}
	return registration.done
}

// Reason returns the bounded reason that closed Done, or None while the
// registration remains attached.
func (registration *Registration) Reason() CancellationReason {
	// Nil registrations are rejected without an attached receiver identity.
	if registration == nil {
		return CancellationReasonRejected
	}
	return CancellationReason(registration.reason.Load())
}

// Unregister idempotently releases indexes and closes the local cancellation
// signal so no registration survives normal stream cleanup.
func (registration *Registration) Unregister() {
	// Rejected and nil registrations have no registry state to release.
	if registration == nil || registration.registry == nil {
		return
	}

	registry := registration.registry
	registry.mu.Lock()
	defer registry.mu.Unlock()
	// A prior invalidation already detached and closed the registration.
	if registry.registrations[registration.id] != registration {
		return
	}
	registry.detachLocked(registration, CancellationReasonUnregistered)
}

// InvalidateToken implements apptokeninvalidation.Invalidator structurally. It
// advances the opaque identity fence and closes all exact-app receiver
// registrations that used it.
func (registry *Registry) InvalidateToken(tokenID uuid.UUID) int {
	// A nil registry or absent identity cannot match an authorized receiver.
	if registry == nil || tokenID == uuid.Nil {
		return 0
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.initializeLocked()
	registry.tokens[tokenID]++
	invalidated := registry.invalidateIndexedLocked(registry.registrationsByToken[tokenID], CancellationReasonTokenInvalidated)
	registry.cleanupTokenGenerationLocked(tokenID)
	return invalidated
}

// InvalidateAppRuntime advances one exact app-version generation and closes
// only its current receivers. A later successful authorization may register
// again after revalidating the unchanged exact identity.
func (registry *Registry) InvalidateAppRuntime(appID uuid.UUID) int {
	// Malformed exact identity cannot be broadened into family-wide invalidation.
	if registry == nil || appID == uuid.Nil {
		return 0
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.initializeLocked()
	registry.apps[appID]++
	invalidated := registry.invalidateIndexedLocked(registry.registrationsByApp[appID], CancellationReasonAppInvalidated)
	registry.cleanupAppGenerationLocked(appID)
	return invalidated
}

// initializeLocked lazily creates every index while Registry's mutex is held.
func (registry *Registry) initializeLocked() {
	// Lazy setup keeps the zero value safe without allocating until first use.
	if registry.tokens != nil {
		return
	}
	registry.tokens = make(map[uuid.UUID]uint64)
	registry.apps = make(map[uuid.UUID]uint64)
	registry.registrations = make(map[uint64]*Registration)
	registry.registrationsByToken = make(map[uuid.UUID]map[uint64]struct{})
	registry.registrationsByApp = make(map[uuid.UUID]map[uint64]struct{})
}

// addIndexLocked adds one registration to an identity index while Registry's
// mutex is held.
func (registry *Registry) addIndexLocked(index map[uuid.UUID]map[uint64]struct{}, identity uuid.UUID, registrationID uint64) {
	registrations := index[identity]
	// Each identity allocates its membership set only when its first stream arrives.
	if registrations == nil {
		registrations = make(map[uint64]struct{})
		index[identity] = registrations
	}
	registrations[registrationID] = struct{}{}
}

// matchesCurrentGenerationLocked validates both captured fences while
// Registry's mutex is held.
func (registry *Registry) matchesCurrentGenerationLocked(registration *Registration) bool {
	return registry.tokens[registration.tokenID] == registration.tokenGeneration &&
		registry.apps[registration.appID] == registration.appGeneration
}

// cancellationReasonForStaleLocked chooses the most specific closed identity
// while Registry's mutex is held.
func cancellationReasonForStaleLocked(registry *Registry, registration *Registration) CancellationReason {
	// Token revocation takes precedence because reauthorization can never make
	// the same opaque identity valid again.
	if registry.tokens[registration.tokenID] != registration.tokenGeneration {
		return CancellationReasonTokenInvalidated
	}
	return CancellationReasonAppInvalidated
}

// invalidateIndexedLocked detaches and closes one snapshot of matching
// registrations while Registry's mutex is held.
func (registry *Registry) invalidateIndexedLocked(indexed map[uint64]struct{}, reason CancellationReason) int {
	invalidated := 0
	// Deleting the current key during map iteration is safe and prevents stale
	// membership from being revisited by a repeated invalidation.
	for registrationID := range indexed {
		registration := registry.registrations[registrationID]
		// A concurrent cleanup may already have removed this cross-index entry.
		if registration == nil {
			continue
		}
		registry.detachLocked(registration, reason)
		invalidated++
	}
	return invalidated
}

// detachLocked removes one registration from every index and closes its local
// cancellation signal while Registry's mutex is held.
func (registry *Registry) detachLocked(registration *Registration, reason CancellationReason) {
	delete(registry.registrations, registration.id)
	registry.removeIndexLocked(registry.registrationsByToken, registration.tokenID, registration.id)
	registry.removeIndexLocked(registry.registrationsByApp, registration.appID, registration.id)
	registry.cleanupTokenGenerationLocked(registration.tokenID)
	registry.cleanupAppGenerationLocked(registration.appID)
	registration.reason.Store(uint32(reason))
	close(registration.done)
}

// removeIndexLocked removes one registration and releases an empty identity
// membership set while Registry's mutex is held.
func (registry *Registry) removeIndexLocked(index map[uuid.UUID]map[uint64]struct{}, identity uuid.UUID, registrationID uint64) {
	registrations := index[identity]
	delete(registrations, registrationID)
	// Empty membership must be removed so normal stream churn stays bounded.
	if len(registrations) == 0 {
		delete(index, identity)
	}
}

// cleanupTokenGenerationLocked releases a token fence once no registration can
// still carry its preceding generation; future callers must reauthorize before
// confirming a newly captured generation.
func (registry *Registry) cleanupTokenGenerationLocked(tokenID uuid.UUID) {
	// An indexed registration still needs the generation to detect invalidation.
	if len(registry.registrationsByToken[tokenID]) != 0 {
		return
	}
	delete(registry.tokens, tokenID)
}

// cleanupAppGenerationLocked releases an app fence once every registration
// from its preceding generation has been detached.
func (registry *Registry) cleanupAppGenerationLocked(appID uuid.UUID) {
	// An indexed registration still needs the generation to detect invalidation.
	if len(registry.registrationsByApp[appID]) != 0 {
		return
	}
	delete(registry.apps, appID)
}

// rejectedRegistration returns a closed handle that cannot accidentally be
// treated as an active stream after validation failure.
func rejectedRegistration() *Registration {
	registration := &Registration{done: make(chan struct{})}
	registration.reason.Store(uint32(CancellationReasonRejected))
	close(registration.done)
	return registration
}

// closedDoneChannel returns a fresh closed signal for a nil registration.
func closedDoneChannel() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
