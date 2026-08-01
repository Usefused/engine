package accesscontrol

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/sync/singleflight"
)

var ErrStaleAuthorizationRevision = errors.New("stale authorization revision")

type ControlPrincipal struct {
	AccountID       uuid.UUID
	WorkspaceID     uuid.UUID
	SubjectID       uuid.UUID
	CredentialID    uuid.UUID
	Kind            SubjectKind
	ExpiresAt       *time.Time
	Revision        int64
	EffectiveGrants []Grant
}

type PrincipalLoader interface {
	LoadControlPrincipal(ctx context.Context, credentialHash string) (ControlPrincipal, error)
}

type AuthenticatorOptions struct {
	CacheEntries     int
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	Now              func() time.Time
}

type Authenticator struct {
	loader          PrincipalLoader
	credentialCache *boundedCache[string, cachedCredential]
	snapshotCache   *boundedCache[string, AuthorizationSnapshot]
	negativeCache   *boundedCache[string, struct{}]
	loadGroup       singleflight.Group
	revision        atomic.Int64
	revisionMu      sync.RWMutex
	now             func() time.Time
}

type cachedCredential struct {
	AccountID    uuid.UUID
	WorkspaceID  uuid.UUID
	SubjectID    uuid.UUID
	CredentialID uuid.UUID
	Kind         SubjectKind
	ExpiresAt    *time.Time
}

func NewAuthenticator(loader PrincipalLoader, revision int64, options AuthenticatorOptions) (*Authenticator, error) {
	if loader == nil || revision <= 0 {
		return nil, fmt.Errorf("invalid authenticator configuration")
	}
	// Keep every caller-supplied cache setting within the security envelope:
	// bounded entries limit credential-spray memory growth, while short TTLs
	// bound stale identity and grant snapshots if revision polling is delayed.
	if options.CacheEntries <= 0 {
		options.CacheEntries = 1024
	}
	if options.CacheTTL <= 0 || options.CacheTTL > 30*time.Second {
		options.CacheTTL = 30 * time.Second
	}
	if options.NegativeCacheTTL <= 0 || options.NegativeCacheTTL > 2*time.Second {
		options.NegativeCacheTTL = time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	authenticator := &Authenticator{
		loader:          loader,
		credentialCache: newBoundedCache[string, cachedCredential](options.CacheEntries, options.CacheTTL, options.Now),
		snapshotCache:   newBoundedCache[string, AuthorizationSnapshot](options.CacheEntries, options.CacheTTL, options.Now),
		negativeCache:   newBoundedCache[string, struct{}](options.CacheEntries, options.NegativeCacheTTL, options.Now),
		now:             options.Now,
	}
	authenticator.revision.Store(revision)
	return authenticator, nil
}

func (a *Authenticator) AuthenticateControlCredential(ctx context.Context, credential string) (Actor, error) {
	started := time.Now()
	outcome := "denied"
	cacheHit := false
	defer func() { recordAuthentication(ctx, started, outcome, cacheHit) }()
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.authn.control")
	defer span.End()
	if credential == "" {
		span.SetStatus(codes.Error, "missing credential")
		return Actor{}, ErrAuthenticationRequired
	}

	credentialHash := HashControlCredential(credential)
	if actor, ok := a.cachedActor(credentialHash); ok {
		outcome = "allowed"
		cacheHit = true
		span.SetAttributes(attribute.Bool("engine.authn.cache_hit", true))
		return actor, nil
	}
	if _, denied := a.negativeCache.get(credentialHash); denied {
		recordNegativeCache(ctx, "hit")
		cacheHit = true
		span.SetAttributes(attribute.Bool("engine.authn.cache_hit", true), attribute.Bool("engine.authn.negative_cache", true))
		span.RecordError(ErrAuthenticationRequired)
		span.SetStatus(codes.Error, "authentication failed")
		return Actor{}, ErrAuthenticationRequired
	}
	recordNegativeCache(ctx, "miss")
	result, err, shared := a.loadPrincipal(ctx, credentialHash)
	if err != nil {
		if result.cacheHit {
			cacheHit = true
			span.SetAttributes(attribute.Bool("engine.authn.cache_hit", true), attribute.Bool("engine.authn.negative_cache", true))
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "authentication failed")
		return Actor{}, err
	}
	actor := result.actor
	cacheHit = result.cacheHit
	span.SetAttributes(
		attribute.Bool("engine.authn.cache_hit", cacheHit),
		attribute.Bool("engine.authn.coalesced", shared),
		attribute.Int("engine.authorization.grants", len(actor.Authorization.grants)),
	)
	outcome = "allowed"
	return actor, nil
}

type principalLoadResult struct {
	actor    Actor
	cacheHit bool
}

func (a *Authenticator) loadPrincipal(ctx context.Context, credentialHash string) (principalLoadResult, error, bool) {
	loaded, err, shared := a.loadGroup.Do(credentialHash, func() (any, error) {
		return a.loadPrincipalOnce(ctx, credentialHash)
	})
	return loaded.(principalLoadResult), err, shared
}

func (a *Authenticator) loadPrincipalOnce(ctx context.Context, credentialHash string) (principalLoadResult, error) {
	// A goroutine may have passed the outer cache check just before another
	// load completed. Recheck both caches inside the coalesced section to avoid
	// a second database read after the first flight published a result.
	if actor, ok := a.cachedActor(credentialHash); ok {
		return principalLoadResult{actor: actor, cacheHit: true}, nil
	}
	if _, denied := a.negativeCache.get(credentialHash); denied {
		recordNegativeCache(ctx, "hit")
		return principalLoadResult{cacheHit: true}, ErrAuthenticationRequired
	}
	observedRevision := a.revision.Load()
	queryStarted := time.Now()
	principal, err := a.loader.LoadControlPrincipal(ctx, credentialHash)
	recordAuthorizationQuery(ctx, "load_principal", queryStarted)
	if errors.Is(err, ErrAuthenticationRequired) {
		a.publishNegativeCredential(credentialHash, observedRevision)
		recordNegativeCache(ctx, "load_denied")
	}
	if err != nil {
		return principalLoadResult{}, err
	}
	actor, err := a.cachePrincipal(credentialHash, principal)
	return principalLoadResult{actor: actor}, err
}

func (a *Authenticator) publishNegativeCredential(credentialHash string, observedRevision int64) {
	a.revisionMu.RLock()
	defer a.revisionMu.RUnlock()
	// A credential can be created while its earlier lookup is in flight. Only
	// publish a denial for the same authorization revision that was queried;
	// otherwise SetRevision has already made that result stale.
	if a.revision.Load() == observedRevision {
		a.negativeCache.set(credentialHash, struct{}{})
	}
}

func (a *Authenticator) SetRevision(revision int64) bool {
	a.revisionMu.Lock()
	defer a.revisionMu.Unlock()
	// Pollers and mutations can report out of order. Authorization revisions are
	// monotonic, so a late older value must never republish stale cache state.
	for {
		current := a.revision.Load()
		if revision <= current {
			return false
		}
		if a.revision.CompareAndSwap(current, revision) {
			break
		}
	}
	// Identity, grants, and negative credential results describe one revision;
	// clearing them together prevents a valid identity from using old grants or
	// a newly issued credential from inheriting an earlier cached denial.
	a.credentialCache.clear()
	a.snapshotCache.clear()
	a.negativeCache.clear()
	return true
}

func (a *Authenticator) CurrentRevision() int64 {
	return a.revision.Load()
}

func (a *Authenticator) cachedActor(credentialHash string) (Actor, bool) {
	a.revisionMu.RLock()
	defer a.revisionMu.RUnlock()
	identity, ok := a.credentialCache.get(credentialHash)
	if !ok || credentialExpired(identity.ExpiresAt, a.now()) {
		return Actor{}, false
	}
	// Snapshot keys include the current revision so a cached identity can never
	// be paired with grants loaded before the latest invalidation.
	snapshot, ok := a.snapshotCache.get(snapshotCacheKey(identity.SubjectID, a.revision.Load()))
	if !ok {
		return Actor{}, false
	}
	return actorFromCached(identity, snapshot), true
}

func (a *Authenticator) cachePrincipal(credentialHash string, principal ControlPrincipal) (Actor, error) {
	if credentialExpired(principal.ExpiresAt, a.now()) {
		return Actor{}, ErrAuthenticationRequired
	}
	currentRevision := a.revision.Load()
	if principal.Revision < currentRevision {
		return Actor{}, fmt.Errorf("%w: loaded %d, current %d", ErrStaleAuthorizationRevision, principal.Revision, currentRevision)
	}
	if principal.Revision > currentRevision {
		a.SetRevision(principal.Revision)
	}
	a.revisionMu.RLock()
	defer a.revisionMu.RUnlock()
	// Serialize the final snapshot publication against revision invalidation.
	// A concurrent revocation therefore either precedes this actor or makes the
	// loaded principal stale; it cannot leave stale cache entries behind.
	if principal.Revision != a.revision.Load() {
		return Actor{}, fmt.Errorf("%w: loaded %d, current %d", ErrStaleAuthorizationRevision, principal.Revision, a.revision.Load())
	}
	snapshot, err := NewAuthorizationSnapshot(principal.Revision, principal.EffectiveGrants...)
	if err != nil {
		return Actor{}, err
	}
	identity := cachedCredential{
		AccountID:    principal.AccountID,
		WorkspaceID:  principal.WorkspaceID,
		SubjectID:    principal.SubjectID,
		CredentialID: principal.CredentialID,
		Kind:         principal.Kind,
		ExpiresAt:    principal.ExpiresAt,
	}
	a.credentialCache.set(credentialHash, identity)
	a.snapshotCache.set(snapshotCacheKey(principal.SubjectID, principal.Revision), snapshot)
	return actorFromCached(identity, snapshot), nil
}

func actorFromCached(identity cachedCredential, snapshot AuthorizationSnapshot) Actor {
	return Actor{
		AccountID:     identity.AccountID,
		WorkspaceID:   identity.WorkspaceID,
		SubjectID:     identity.SubjectID,
		CredentialID:  identity.CredentialID,
		Kind:          identity.Kind,
		Authorization: snapshot,
	}
}

func credentialExpired(expiresAt *time.Time, now time.Time) bool {
	return expiresAt != nil && !expiresAt.After(now)
}

func snapshotCacheKey(subjectID uuid.UUID, revision int64) string {
	return subjectID.String() + ":" + strconv.FormatInt(revision, 10)
}
