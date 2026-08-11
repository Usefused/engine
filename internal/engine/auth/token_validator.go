package auth

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var (
	ErrUnauthorized = errors.New("unauthorized: invalid app token")
)

type TokenValidator interface {
	// Validate resolves the immutable app and its family from one token lookup.
	// Control-plane credentials do not cross this runtime boundary; only a token
	// issued for the selected app's family is accepted.
	Validate(ctx context.Context, appID uuid.UUID, token string) (RuntimeIdentity, error)
}

type AppAuthorizer interface {
	AuthorizeApp(context.Context, uuid.UUID, string) (*store.AuthProjection, error)
}

// RuntimeIdentity carries only safe identity metadata from authorization into
// execution receipts. Bucket, selection, and token data remain inside the
// authorization/store boundary.
type RuntimeIdentity struct {
	AccountID   uuid.UUID
	AppFamilyID uuid.UUID
	AppID       uuid.UUID
	AppVersion  string
	Kind        store.AppKind
	Status      store.AppStatus
	TokenPolicy store.AppTokenPolicy
	// The validator builds this immutable set once so every tool dispatch does
	// not linearly scan the persisted wire-order slice.
	allowedOperations map[string]struct{}
}

const (
	tokenCacheTTL         = 30 * time.Second
	tokenCacheLoadTimeout = 5 * time.Second
)

type tokenCacheResult string

const (
	tokenCacheResultBypass    tokenCacheResult = "bypass"
	tokenCacheResultHit       tokenCacheResult = "hit"
	tokenCacheResultMiss      tokenCacheResult = "miss"
	tokenCacheResultCoalesced tokenCacheResult = "coalesced"
	tokenCacheResultAttribute                  = "auth.cache.result"
)

type tokenCacheKey struct {
	appID       uuid.UUID
	tokenDigest [sha256.Size]byte
}

type tokenCacheEntry struct {
	identity  RuntimeIdentity
	tokenID   uuid.UUID
	expiresAt time.Time
	cacheID   uint64
}

type tokenCacheExpiry struct {
	key       tokenCacheKey
	expiresAt time.Time
	cacheID   uint64
}

type tokenCacheExpiryHeap []tokenCacheExpiry

func (h tokenCacheExpiryHeap) Len() int           { return len(h) }
func (h tokenCacheExpiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h tokenCacheExpiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *tokenCacheExpiryHeap) Push(value any)    { *h = append(*h, value.(tokenCacheExpiry)) }
func (h *tokenCacheExpiryHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

type tokenLoad struct {
	done       chan struct{}
	identity   RuntimeIdentity
	err        error
	generation uint64
}

type tokenCacheLookup struct {
	identity RuntimeIdentity
	load     *tokenLoad
	hit      bool
	leader   bool
}

// CachedTokenValidator keeps the runtime authorization cache behind the same
// validator used by every SDK and MCP execution path.
type CachedTokenValidator struct {
	store AppAuthorizer
	now   func() time.Time

	mu             sync.Mutex
	entries        map[tokenCacheKey]tokenCacheEntry
	keysByToken    map[uuid.UUID]map[tokenCacheKey]struct{}
	loads          map[tokenCacheKey]*tokenLoad
	expiries       tokenCacheExpiryHeap
	cacheID        uint64
	invalidationID uint64
	invalidations  map[uuid.UUID]uint64
}

func NewTokenValidator(s AppAuthorizer) *CachedTokenValidator {
	return newTokenValidator(s, time.Now)
}

func newTokenValidator(s AppAuthorizer, now func() time.Time) *CachedTokenValidator {
	return &CachedTokenValidator{
		store: s, now: now, entries: make(map[tokenCacheKey]tokenCacheEntry),
		keysByToken:   make(map[uuid.UUID]map[tokenCacheKey]struct{}),
		loads:         make(map[tokenCacheKey]*tokenLoad),
		invalidations: make(map[uuid.UUID]uint64),
	}
}

func (v *CachedTokenValidator) Validate(ctx context.Context, appID uuid.UUID, token string) (RuntimeIdentity, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.auth.app_token.validate")
	defer span.End()

	token = strings.TrimSpace(token)
	if token == "" {
		span.SetAttributes(attribute.String(tokenCacheResultAttribute, string(tokenCacheResultBypass)))
		return RuntimeIdentity{}, ErrUnauthorized
	}

	key := tokenCacheKey{appID: appID, tokenDigest: digestToken(token)}
	identity, result, err := v.validateKey(ctx, key)
	span.SetAttributes(attribute.String(tokenCacheResultAttribute, string(result)))
	return identity, err
}

func (v *CachedTokenValidator) validateKey(ctx context.Context, key tokenCacheKey) (RuntimeIdentity, tokenCacheResult, error) {
	lookup := v.acquire(key)
	if lookup.hit {
		return lookup.identity, tokenCacheResultHit, nil
	}
	if !lookup.leader {
		return waitForTokenLoad(ctx, lookup.load, tokenCacheResultCoalesced)
	}
	// A shared lookup must outlive any one caller, but remains tightly bounded
	// so a broken database cannot leak authorization goroutines.
	go v.loadAndFinish(context.WithoutCancel(ctx), key, lookup.load)
	return waitForTokenLoad(ctx, lookup.load, tokenCacheResultMiss)
}

func (v *CachedTokenValidator) acquire(key tokenCacheKey) tokenCacheLookup {
	now := v.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sweepExpiredLocked(now)
	if entry, ok := v.entries[key]; ok {
		if now.Before(entry.expiresAt) {
			return tokenCacheLookup{identity: cloneRuntimeIdentity(entry.identity), hit: true}
		}
		v.removeEntryLocked(key, entry.tokenID)
	}
	if load, ok := v.loads[key]; ok {
		return tokenCacheLookup{load: load}
	}
	load := &tokenLoad{done: make(chan struct{}), generation: v.invalidationID}
	v.loads[key] = load
	return tokenCacheLookup{load: load, leader: true}
}

func (v *CachedTokenValidator) loadAndFinish(ctx context.Context, key tokenCacheKey, load *tokenLoad) {
	loadCtx, cancel := context.WithTimeout(ctx, tokenCacheLoadTimeout)
	defer cancel()
	identity, tokenID, expiresAt, err := v.load(loadCtx, key)
	v.finishLoad(key, load, identity, tokenID, expiresAt, err)
}

func (v *CachedTokenValidator) load(ctx context.Context, key tokenCacheKey) (RuntimeIdentity, uuid.UUID, time.Time, error) {
	// PostgreSQL stores the portable hex encoding, while the hot in-memory path
	// keeps the fixed-size digest to avoid allocating a string on every hit.
	tokenHash := hex.EncodeToString(key.tokenDigest[:])
	projection, err := v.store.AuthorizeApp(ctx, key.appID, tokenHash)
	if err != nil {
		return RuntimeIdentity{}, uuid.Nil, time.Time{}, ErrUnauthorized
	}
	identity := RuntimeIdentity{
		AccountID: projection.AccountID, AppFamilyID: projection.AppFamilyID,
		AppID: projection.AppID, AppVersion: projection.Version,
		Kind: projection.Kind, Status: projection.AppStatus, TokenPolicy: projection.TokenPolicy,
	}
	if !projection.TokenPolicy.IsUnrestricted() {
		identity.allowedOperations = make(map[string]struct{}, len(projection.TokenPolicy.AllowedOperations))
		for _, operation := range projection.TokenPolicy.AllowedOperations {
			identity.allowedOperations[operation] = struct{}{}
		}
	}
	expiresAt := v.now().Add(tokenCacheTTL)
	if projection.TokenPolicy.ExpiresAt != nil && projection.TokenPolicy.ExpiresAt.Before(expiresAt) {
		expiresAt = *projection.TokenPolicy.ExpiresAt
	}
	if !v.now().Before(expiresAt) {
		return RuntimeIdentity{}, uuid.Nil, time.Time{}, ErrUnauthorized
	}
	return identity, projection.TokenID, expiresAt, nil
}

func (v *CachedTokenValidator) finishLoad(key tokenCacheKey, load *tokenLoad, identity RuntimeIdentity, tokenID uuid.UUID, expiresAt time.Time, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// A DELETE that committed after this lookup began makes its snapshot stale.
	// Poison only that token's shared result; unrelated revocations do not make
	// valid callers retry PostgreSQL.
	if v.invalidations[tokenID] > load.generation {
		identity, err = RuntimeIdentity{}, ErrUnauthorized
	}
	load.identity, load.err = cloneRuntimeIdentity(identity), err
	if err == nil && tokenID != uuid.Nil {
		v.addEntryLocked(key, tokenCacheEntry{identity: cloneRuntimeIdentity(identity), tokenID: tokenID, expiresAt: expiresAt})
	}
	delete(v.loads, key)
	v.cleanupInvalidationsLocked()
	close(load.done)
}

func waitForTokenLoad(ctx context.Context, load *tokenLoad, result tokenCacheResult) (RuntimeIdentity, tokenCacheResult, error) {
	select {
	case <-ctx.Done():
		return RuntimeIdentity{}, result, ctx.Err()
	case <-load.done:
		return cloneRuntimeIdentity(load.identity), result, load.err
	}
}

func (v *CachedTokenValidator) addEntryLocked(key tokenCacheKey, entry tokenCacheEntry) {
	v.cacheID++
	entry.cacheID = v.cacheID
	v.entries[key] = entry
	heap.Push(&v.expiries, tokenCacheExpiry{key: key, expiresAt: entry.expiresAt, cacheID: entry.cacheID})
	keys := v.keysByToken[entry.tokenID]
	if keys == nil {
		keys = make(map[tokenCacheKey]struct{})
		v.keysByToken[entry.tokenID] = keys
	}
	keys[key] = struct{}{}
}

// InvalidateToken evicts only the revoked token across every immutable app
// version. The TTL remains a fallback for a missed invalidation message.
func (v *CachedTokenValidator) InvalidateToken(tokenID uuid.UUID) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.invalidationID++
	v.invalidations[tokenID] = v.invalidationID
	invalidated := len(v.keysByToken[tokenID])
	for key := range v.keysByToken[tokenID] {
		delete(v.entries, key)
	}
	delete(v.keysByToken, tokenID)
	v.cleanupInvalidationsLocked()
	return invalidated
}

func (v *CachedTokenValidator) sweepExpiredLocked(now time.Time) {
	for len(v.expiries) > 0 && !now.Before(v.expiries[0].expiresAt) {
		expired := heap.Pop(&v.expiries).(tokenCacheExpiry)
		entry, ok := v.entries[expired.key]
		// Exact invalidation leaves a harmless heap tombstone. The insertion ID
		// prevents it from removing a newer replacement with the same expiry.
		if ok && entry.cacheID == expired.cacheID {
			v.removeEntryLocked(expired.key, entry.tokenID)
		}
	}
}

func (v *CachedTokenValidator) cleanupInvalidationsLocked() {
	if len(v.loads) == 0 {
		clear(v.invalidations)
		return
	}
	oldest := v.invalidationID
	for _, load := range v.loads {
		if load.generation < oldest {
			oldest = load.generation
		}
	}
	for tokenID, generation := range v.invalidations {
		if generation <= oldest {
			delete(v.invalidations, tokenID)
		}
	}
}

func (v *CachedTokenValidator) removeEntryLocked(key tokenCacheKey, tokenID uuid.UUID) {
	delete(v.entries, key)
	keys := v.keysByToken[tokenID]
	delete(keys, key)
	if len(keys) == 0 {
		delete(v.keysByToken, tokenID)
	}
}

func cloneRuntimeIdentity(identity RuntimeIdentity) RuntimeIdentity {
	identity.TokenPolicy.AllowedOperations = append([]string(nil), identity.TokenPolicy.AllowedOperations...)
	if identity.TokenPolicy.ExpiresAt != nil {
		expiresAt := *identity.TokenPolicy.ExpiresAt
		identity.TokenPolicy.ExpiresAt = &expiresAt
	}
	return identity
}

// AllowsOperation applies the token's deny-all-except rule. App-version scope
// is checked separately, so this helper cannot grant an unselected operation.
func (identity RuntimeIdentity) AllowsOperation(operation string) bool {
	if identity.TokenPolicy.IsUnrestricted() {
		return true
	}
	if identity.allowedOperations != nil {
		_, allowed := identity.allowedOperations[operation]
		return allowed
	}
	return identity.TokenPolicy.AllowsOperation(operation)
}

func HashToken(token string) string {
	hash := digestToken(token)
	return hex.EncodeToString(hash[:])
}

func digestToken(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}
