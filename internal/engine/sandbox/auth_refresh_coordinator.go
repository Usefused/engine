package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultAuthRefreshHTTPTimeout         = 15 * time.Second
	defaultForegroundRefreshLease         = 30 * time.Second
	defaultForegroundRefreshWait          = 20 * time.Second
	defaultForegroundRefreshPollInterval  = 50 * time.Millisecond
	defaultTransientRefreshRetryDelay     = 5 * time.Minute
	defaultContractRefreshRetryDelay      = time.Hour
	defaultAuthRefreshAttemptActiveWindow = 15 * time.Minute
	defaultAuthRefreshLeaseFinishReserve  = 5 * time.Second
	defaultAuthRefreshProviderStartBuffer = time.Second
	defaultAuthRefreshSuccessInterval     = time.Hour
	defaultAuthRefreshExpirySafetyMargin  = 10 * time.Minute
	defaultAuthRefreshMinimumCooldown     = time.Minute
)

// ErrAuthRefreshFailed is the sanitized error returned to worker and request
// callers when provider or persistence recovery remains retryable.
var ErrAuthRefreshFailed = errors.New("connected auth refresh failed")

// ErrAuthRefreshContractUnavailable is the sanitized error returned when the
// immutable auth contract needed for refresh cannot be loaded safely.
var ErrAuthRefreshContractUnavailable = errors.New("connected auth refresh contract unavailable")

// AuthRefreshOutcome is a bounded refresh result safe for metrics and logs.
type AuthRefreshOutcome string

const (
	AuthRefreshOutcomeRefreshed           AuthRefreshOutcome = "refreshed"
	AuthRefreshOutcomeNotDue              AuthRefreshOutcome = "not_due"
	AuthRefreshOutcomeLeaseContended      AuthRefreshOutcome = "lease_contended"
	AuthRefreshOutcomeTransientFailure    AuthRefreshOutcome = "transient_failure"
	AuthRefreshOutcomeReconnectRequired   AuthRefreshOutcome = "reconnect_required"
	AuthRefreshOutcomeContractUnavailable AuthRefreshOutcome = "contract_unavailable"
)

// AuthRefreshResult deliberately exposes only low-cardinality decisions and
// never provider responses, connection rows, or credential material.
type AuthRefreshResult struct {
	Outcome     AuthRefreshOutcome
	FailureCode string
}

// AuthRefreshCoordinatorOption configures deterministic coordinator dependencies.
type AuthRefreshCoordinatorOption func(*AuthRefreshCoordinator)

// AuthRefreshCoordinator serializes OAuth rotation through store-owned leases
// while keeping provider network I/O outside database transactions.
type AuthRefreshCoordinator struct {
	db                     store.Store
	refreshStore           store.AuthConnectionRefreshStore
	masterKey              []byte
	applicationCredentials *connectauth.ApplicationCredentialResolver
	httpClient             *http.Client
	now                    func() time.Time
	foregroundLease        time.Duration
	foregroundWait         time.Duration
	foregroundPollInterval time.Duration
}

// authRefreshAttempt keeps the public worker result separate from the saved
// encrypted row needed by foreground dispatch after a successful refresh.
type authRefreshAttempt struct {
	connection *store.AuthConnection
	result     AuthRefreshResult
	err        error
}

// authRefreshContract contains the exact immutable provider contract and the
// decrypted workspace client registration needed for one token exchange.
type authRefreshContract struct {
	auth  fusedobject.AuthConfig
	flow  fusedobject.OAuth2FlowContract
	creds connectauth.ClientCredentials
}

// authRefreshContractStore keeps refresh metadata reads narrower than the full
// snapshot repository used by runtime operation loading.
type authRefreshContractStore interface {
	GetServiceContractMetadata(context.Context, uuid.UUID, uuid.UUID) (*fusedobject.ServiceMetadata, error)
}

// NewAuthRefreshCoordinator builds the shared worker and foreground executor.
func NewAuthRefreshCoordinator(db store.Store, masterKey []byte, opts ...AuthRefreshCoordinatorOption) *AuthRefreshCoordinator {
	refreshStore, _ := db.(store.AuthConnectionRefreshStore)
	coordinator := &AuthRefreshCoordinator{
		db:                     db,
		refreshStore:           refreshStore,
		masterKey:              append([]byte(nil), masterKey...),
		applicationCredentials: connectauth.NewApplicationCredentialResolver(db, masterKey, ""),
		httpClient:             &http.Client{Timeout: defaultAuthRefreshHTTPTimeout},
		now:                    func() time.Time { return time.Now().UTC() },
		foregroundLease:        defaultForegroundRefreshLease,
		foregroundWait:         defaultForegroundRefreshWait,
		foregroundPollInterval: defaultForegroundRefreshPollInterval,
	}
	for _, opt := range opts {
		opt(coordinator)
	}
	return coordinator
}

// WithAuthRefreshHTTPClient replaces the provider client for deterministic tests.
func WithAuthRefreshHTTPClient(client *http.Client) AuthRefreshCoordinatorOption {
	return func(coordinator *AuthRefreshCoordinator) {
		if client != nil {
			coordinator.httpClient = client
		}
	}
}

// WithAuthRefreshClock replaces wall time for deterministic expiry and retry tests.
func WithAuthRefreshClock(now func() time.Time) AuthRefreshCoordinatorOption {
	return func(coordinator *AuthRefreshCoordinator) {
		if now != nil {
			coordinator.now = now
		}
	}
}

// withAuthRefreshForegroundTiming shortens only foreground coordination waits in tests.
func withAuthRefreshForegroundTiming(lease, wait, poll time.Duration) AuthRefreshCoordinatorOption {
	return func(coordinator *AuthRefreshCoordinator) {
		coordinator.foregroundLease = lease
		coordinator.foregroundWait = wait
		coordinator.foregroundPollInterval = poll
	}
}

// RefreshClaimedConnection refreshes one already-leased connection and returns
// only a bounded result suitable for worker telemetry.
func (c *AuthRefreshCoordinator) RefreshClaimedConnection(ctx context.Context, claim store.AuthConnectionRefreshClaim) (AuthRefreshResult, error) {
	attempt := c.refreshClaimedConnection(ctx, claim, "worker")
	return attempt.result, sanitizedAuthRefreshError(attempt)
}

// ensureForegroundConnectionFresh applies the five-minute request fallback
// while coordinating rotation with all workers and Engine replicas through CAS.
func (c *AuthRefreshCoordinator) ensureForegroundConnectionFresh(ctx context.Context, conn *store.AuthConnection) (*store.AuthConnection, error) {
	// Refresh must fail closed before token use if an in-memory caller bypasses the clean persistence boundary.
	if conn == nil || conn.ServiceVersionID == uuid.Nil {
		return nil, ErrAuthRefreshContractUnavailable
	}
	now := c.now().UTC()
	// A token outside the proactive window can be used without acquiring a refresh lease.
	if !authConnectionNeedsRefresh(conn, now) {
		recordUnclaimedAuthRefreshDecision(ctx, conn, "request", AuthRefreshResult{Outcome: AuthRefreshOutcomeNotDue})
		return conn, nil
	}
	// Refresh is unavailable when the composed store cannot provide durable CAS operations.
	if c.refreshStore == nil {
		return foregroundTransientResult(conn, now, ErrAuthRefreshFailed)
	}
	latest, decided, err := c.foregroundConnectionBeforeClaim(ctx, conn, now)
	// A durable reconnect, throttle, or concurrent completion can resolve the request before a new claim.
	if decided {
		return latest, err
	}
	conn = latest
	claim, err := c.refreshStore.TryClaimAuthConnectionRefresh(ctx, conn.ID, now, now.Add(c.foregroundLease))
	// Store failures remain transient so callers never receive raw persistence diagnostics.
	if err != nil {
		return foregroundTransientResult(conn, now, ErrAuthRefreshFailed)
	}
	// A missing claim normally means another Engine owns the lease; the reload path distinguishes the exact state.
	if claim == nil {
		return c.handleForegroundClaimMiss(ctx, conn, now)
	}
	attempt := c.refreshClaimedConnection(ctx, *claim, "request")
	return c.foregroundAttemptResult(conn, c.now().UTC(), attempt)
}

// foregroundConnectionBeforeClaim reloads durable state and resolves every
// non-claim request decision before attempting cross-replica lease admission.
func (c *AuthRefreshCoordinator) foregroundConnectionBeforeClaim(ctx context.Context, original *store.AuthConnection, now time.Time) (*store.AuthConnection, bool, error) {
	// Why: reload before claiming so a request carrying a stale connection copy
	// cannot rotate again after another owner has already completed refresh.
	latest, err := c.refreshStore.GetAuthConnectionByID(ctx, original.ID)
	if err != nil || latest == nil {
		connection, resultErr := foregroundTransientResult(original, now, ErrAuthRefreshFailed)
		return connection, true, resultErr
	}
	if latest.RefreshState == reconnectRequiredCode {
		return nil, true, foregroundConnectionStateError(latest)
	}
	if authConnectionAccessOnlyAndValid(latest, now) {
		// Why: without refresh material, proactive renewal is impossible; the
		// current access token remains usable until its provider expiry.
		recordUnclaimedAuthRefreshDecision(ctx, latest, "request", AuthRefreshResult{Outcome: AuthRefreshOutcomeNotDue, FailureCode: "refresh_token_missing_access_valid"})
		return latest, true, nil
	}
	if !authConnectionNeedsRefresh(latest, now) {
		recordUnclaimedAuthRefreshDecision(ctx, latest, "request", AuthRefreshResult{Outcome: AuthRefreshOutcomeNotDue})
		return latest, true, nil
	}
	if authConnectionRefreshRetryScheduled(latest, now) {
		// Why: an earlier transient attempt already established the next safe
		// retry time, so requests cannot create an expired-token retry loop.
		recordUnclaimedAuthRefreshDecision(ctx, latest, "request", AuthRefreshResult{Outcome: AuthRefreshOutcomeNotDue, FailureCode: "refresh_retry_deferred"})
		connection, resultErr := foregroundTransientResult(latest, now, ErrAuthRefreshFailed)
		return connection, true, resultErr
	}
	return latest, false, nil
}

// handleForegroundClaimMiss distinguishes a retry throttle or invariant miss
// from a genuinely active refresh before deciding whether polling is useful.
func (c *AuthRefreshCoordinator) handleForegroundClaimMiss(ctx context.Context, original *store.AuthConnection, now time.Time) (*store.AuthConnection, error) {
	connection, err := c.refreshStore.GetAuthConnectionByID(ctx, original.ID)
	if err != nil || connection == nil {
		return foregroundTransientResult(original, now, ErrAuthRefreshFailed)
	}
	if connection.RefreshState == reconnectRequiredCode {
		return nil, foregroundConnectionStateError(connection)
	}
	if authConnectionAccessOnlyAndValid(connection, now) {
		recordUnclaimedAuthRefreshDecision(ctx, connection, "request", AuthRefreshResult{Outcome: AuthRefreshOutcomeNotDue, FailureCode: "refresh_token_missing_access_valid"})
		return connection, nil
	}
	if !authConnectionNeedsRefresh(connection, now) {
		recordUnclaimedAuthRefreshDecision(ctx, connection, "request", AuthRefreshResult{Outcome: AuthRefreshOutcomeNotDue})
		return connection, nil
	}
	if authConnectionRefreshRetryScheduled(connection, now) {
		recordUnclaimedAuthRefreshDecision(ctx, connection, "request", AuthRefreshResult{Outcome: AuthRefreshOutcomeNotDue, FailureCode: "refresh_retry_deferred"})
		return foregroundTransientResult(connection, now, ErrAuthRefreshFailed)
	}
	if !authConnectionRefreshAttemptActive(connection, now) {
		recordUnclaimedAuthRefreshDecision(ctx, connection, "request", transientRefreshResult("refresh_claim_unavailable"))
		return foregroundTransientResult(connection, now, ErrAuthRefreshFailed)
	}
	recordUnclaimedAuthRefreshDecision(ctx, connection, "request", AuthRefreshResult{Outcome: AuthRefreshOutcomeLeaseContended, FailureCode: "refresh_lease_contended"})
	return c.waitForForegroundRefresh(ctx, connection)
}

// refreshClaimedConnection owns exactly one provider exchange and completes or
// releases the matching lease without holding a database lock during HTTP I/O.
func (c *AuthRefreshCoordinator) refreshClaimedConnection(ctx context.Context, claim store.AuthConnectionRefreshClaim, trigger string) (attempt authRefreshAttempt) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.auth_connection.refresh")
	defer func() {
		recordAuthRefreshSpanDecision(span, trigger, attempt.result)
		span.End()
	}()
	span.SetAttributes(authConnectionSpanAttrs(&claim.Connection)...)

	if c.refreshStore == nil {
		return authRefreshAttempt{result: transientRefreshResult("refresh_store_unavailable"), err: ErrAuthRefreshFailed}
	}
	if err := validateAuthRefreshClaim(claim); err != nil {
		span.SetStatus(codes.Error, "invalid refresh claim")
		return authRefreshAttempt{result: transientRefreshResult("invalid_refresh_claim"), err: err}
	}
	workCtx, finalizeCtx, cancel, ok := c.contextsForAuthRefreshClaim(ctx, claim)
	if !ok {
		// Why: provider I/O after the lease budget is exhausted can overlap a
		// cross-replica reclaim and exchange the same rotating token twice.
		return authRefreshAttempt{result: transientRefreshResult("refresh_lease_expired"), err: ErrAuthRefreshFailed}
	}
	defer cancel()
	attemptStartedAt := c.now().UTC()
	if unusable, done := c.claimRefreshTokenState(finalizeCtx, claim, attemptStartedAt); done {
		return unusable
	}
	contract, err := c.loadAuthRefreshContract(workCtx, &claim.Connection)
	if err != nil {
		return c.releaseClaim(finalizeCtx, claim, AuthRefreshOutcomeContractUnavailable, "refresh_contract_unavailable", defaultContractRefreshRetryDelay, err)
	}
	refreshToken, err := decryptAuthConnectionToken(c.masterKey, claim.Connection.EncryptedDEK, claim.Connection.EncryptedRefreshToken)
	if err != nil {
		return c.releaseClaim(finalizeCtx, claim, AuthRefreshOutcomeTransientFailure, "refresh_token_decrypt_failed", defaultTransientRefreshRetryDelay, err)
	}
	if !c.providerRefreshBudgetAvailable(workCtx) {
		// Why: the token exchange must have time to reach its own HTTP timeout
		// and still leave a bounded persistence window before lease expiry.
		return c.releaseClaim(finalizeCtx, claim, AuthRefreshOutcomeTransientFailure, "refresh_lease_budget_exhausted", defaultTransientRefreshRetryDelay, ErrAuthRefreshFailed)
	}
	token, err := connectauth.RefreshAccessToken(workCtx, c.httpClient, contract.auth, contract.flow, contract.creds, refreshToken)
	completedAt := c.now().UTC()
	if err != nil {
		return c.handleProviderRefreshError(finalizeCtx, claim, completedAt, err)
	}
	updated, err := c.connectionFromRefreshToken(&claim.Connection, token, refreshToken, completedAt)
	if err != nil {
		return c.releaseClaim(finalizeCtx, claim, AuthRefreshOutcomeTransientFailure, "refreshed_token_encrypt_failed", defaultTransientRefreshRetryDelay, err)
	}
	return c.completeClaim(finalizeCtx, claim, updated, completedAt)
}

// claimRefreshTokenState converts known unusable refresh material into the
// durable typed reconnect decision before contract or provider work begins.
func (c *AuthRefreshCoordinator) claimRefreshTokenState(ctx context.Context, claim store.AuthConnectionRefreshClaim, now time.Time) (authRefreshAttempt, bool) {
	if authConnectionRefreshTokenExpired(&claim.Connection, now) {
		return c.markClaimReconnectRequired(ctx, claim, "refresh_token_expired", now), true
	}
	if strings.TrimSpace(claim.Connection.EncryptedRefreshToken) == "" {
		return c.markClaimReconnectRequired(ctx, claim, "refresh_token_missing", now), true
	}
	return authRefreshAttempt{}, false
}

// contextsForAuthRefreshClaim bounds provider work before the persistence
// reserve while giving the terminal CAS its own deadline at lease expiry.
func (c *AuthRefreshCoordinator) contextsForAuthRefreshClaim(ctx context.Context, claim store.AuthConnectionRefreshClaim) (context.Context, context.Context, context.CancelFunc, bool) {
	remaining := claim.LeaseExpiresAt.Sub(c.now().UTC())
	if remaining <= defaultAuthRefreshLeaseFinishReserve {
		return nil, nil, nil, false
	}
	workCtx, cancelWork := context.WithTimeout(ctx, remaining-defaultAuthRefreshLeaseFinishReserve)
	// Why: once a provider may have consumed a rotating refresh token, caller
	// cancellation must not prevent the short lease-owned CAS cleanup.
	finalizeCtx, cancelFinalize := context.WithTimeout(context.WithoutCancel(ctx), remaining)
	cancel := func() {
		cancelWork()
		cancelFinalize()
	}
	return workCtx, finalizeCtx, cancel, true
}

// providerRefreshBudgetAvailable refuses a new exchange unless the HTTP client
// can time out before the claim context's reserved completion boundary.
func (c *AuthRefreshCoordinator) providerRefreshBudgetAvailable(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	httpTimeout := c.httpClient.Timeout
	if httpTimeout <= 0 {
		httpTimeout = defaultAuthRefreshHTTPTimeout
	}
	return time.Until(deadline) > httpTimeout+defaultAuthRefreshProviderStartBuffer
}

// recordAuthRefreshSpanDecision emits only bounded decision metadata for every
// claimed attempt, regardless of which internal branch completed it.
func recordAuthRefreshSpanDecision(span trace.Span, trigger string, result AuthRefreshResult) {
	span.SetAttributes(
		attribute.String("auth.refresh.trigger", trigger),
		attribute.String("auth.refresh.outcome", string(result.Outcome)),
		attribute.String("auth.refresh.failure_code", result.FailureCode),
	)
	if authRefreshOutcomeIsSuccessful(result.Outcome) {
		span.SetStatus(codes.Ok, string(result.Outcome))
		return
	}
	span.SetStatus(codes.Error, string(result.Outcome))
}

// recordUnclaimedAuthRefreshDecision closes telemetry gaps for request paths
// that intentionally skip provider exchange and therefore have no claimed span.
func recordUnclaimedAuthRefreshDecision(ctx context.Context, conn *store.AuthConnection, trigger string, result AuthRefreshResult) {
	_, span := otel.Tracer("engine").Start(ctx, "engine.auth_connection.refresh")
	span.SetAttributes(authConnectionSpanAttrs(conn)...)
	recordAuthRefreshSpanDecision(span, trigger, result)
	span.End()
}

// authRefreshOutcomeIsSuccessful keeps expected coordination decisions from
// being reported as span failures while retaining their exact bounded outcome.
func authRefreshOutcomeIsSuccessful(outcome AuthRefreshOutcome) bool {
	return outcome == AuthRefreshOutcomeRefreshed || outcome == AuthRefreshOutcomeNotDue || outcome == AuthRefreshOutcomeLeaseContended
}

// validateAuthRefreshClaim rejects incomplete worker input before credential decryption.
func validateAuthRefreshClaim(claim store.AuthConnectionRefreshClaim) error {
	if claim.Connection.ID == uuid.Nil || claim.Connection.ServiceID == uuid.Nil || claim.Connection.ServiceVersionID == uuid.Nil || claim.LeaseToken == uuid.Nil {
		return errors.New("refresh claim identity is incomplete")
	}
	return nil
}

// loadAuthRefreshContract selects the exact version-pinned auth definition and
// verifies the bucket registration still targets the same named auth slot.
func (c *AuthRefreshCoordinator) loadAuthRefreshContract(ctx context.Context, conn *store.AuthConnection) (authRefreshContract, error) {
	contractStore, ok := c.db.(authRefreshContractStore)
	if !ok {
		return authRefreshContract{}, ErrAuthRefreshContractUnavailable
	}
	metadata, err := contractStore.GetServiceContractMetadata(ctx, conn.ServiceID, conn.ServiceVersionID)
	if err != nil || metadata == nil {
		return authRefreshContract{}, ErrAuthRefreshContractUnavailable
	}
	auth, flow, err := connectedRefreshAuth(conn.AuthName, metadata.AuthConfigs)
	if err != nil || !refreshAuthMatchesConnection(auth, conn) {
		return authRefreshContract{}, ErrAuthRefreshContractUnavailable
	}
	source := connectauth.ApplicationCredentialSource{
		ServiceID: conn.CredentialSourceServiceID,
		AuthType:  conn.CredentialSourceAuthType,
		AuthName:  conn.CredentialSourceAuthName,
	}
	creds, err := c.applicationCredentials.Resolve(ctx, conn.BucketID, conn.ServiceID, conn.AuthType, conn.AuthName, source)
	if err != nil {
		return authRefreshContract{}, ErrAuthRefreshContractUnavailable
	}
	return authRefreshContract{auth: auth, flow: flow, creds: creds}, nil
}

// refreshAuthMatchesConnection prevents a same-name auth definition from a
// different OAuth/OIDC family being substituted into the persisted grant.
func refreshAuthMatchesConnection(auth fusedobject.AuthConfig, conn *store.AuthConnection) bool {
	return canonicalFusedAuthType(auth) == canonicalAuthConfigType(conn.AuthType, "")
}

// handleProviderRefreshError separates permanent user-grant rejection from
// retryable provider and transport failures without persisting provider text.
func (c *AuthRefreshCoordinator) handleProviderRefreshError(ctx context.Context, claim store.AuthConnectionRefreshClaim, now time.Time, refreshErr error) authRefreshAttempt {
	if connectauth.IsReconnectRequiredRefreshError(refreshErr) {
		return c.markClaimReconnectRequired(ctx, claim, "refresh_token_rejected", now)
	}
	return c.releaseClaim(ctx, claim, AuthRefreshOutcomeTransientFailure, "provider_refresh_failed", defaultTransientRefreshRetryDelay, refreshErr)
}

// markClaimReconnectRequired atomically records the stable consent decision for
// the lease owner and never rewrites encrypted token fields.
func (c *AuthRefreshCoordinator) markClaimReconnectRequired(ctx context.Context, claim store.AuthConnectionRefreshClaim, failureCode string, now time.Time) authRefreshAttempt {
	applied, err := c.refreshStore.MarkAuthConnectionReconnectRequired(ctx, claim.Connection.ID, claim.LeaseToken, failureCode, executionTraceID(ctx), now)
	if err != nil {
		return authRefreshAttempt{result: transientRefreshResult("reconnect_state_write_failed"), err: err}
	}
	if !applied {
		return c.resultAfterStaleClaim(ctx, claim.Connection.ID)
	}
	connection := claim.Connection
	connection.RefreshState = reconnectRequiredCode
	connection.LastFailureCode = failureCode
	connection.LastFailureAt = &now
	return authRefreshAttempt{connection: &connection, result: AuthRefreshResult{Outcome: AuthRefreshOutcomeReconnectRequired, FailureCode: failureCode}}
}

// releaseClaim persists only a stable failure code and retry boundary before
// returning a bounded result to the scheduler.
func (c *AuthRefreshCoordinator) releaseClaim(ctx context.Context, claim store.AuthConnectionRefreshClaim, outcome AuthRefreshOutcome, failureCode string, retryDelay time.Duration, cause error) authRefreshAttempt {
	now := c.now().UTC()
	applied, err := c.refreshStore.ReleaseAuthConnectionRefresh(ctx, claim.Connection.ID, claim.LeaseToken, now.Add(retryDelay), failureCode, executionTraceID(ctx), now)
	if err != nil {
		return authRefreshAttempt{result: transientRefreshResult("refresh_release_failed"), err: err}
	}
	if !applied {
		return c.resultAfterStaleClaim(ctx, claim.Connection.ID)
	}
	return authRefreshAttempt{connection: &claim.Connection, result: AuthRefreshResult{Outcome: outcome, FailureCode: failureCode}, err: cause}
}

// completeClaim commits rotated token material only when this caller still owns
// the lease, preventing a late provider response from overwriting newer tokens.
func (c *AuthRefreshCoordinator) completeClaim(ctx context.Context, claim store.AuthConnectionRefreshClaim, updated store.AuthConnection, refreshedAt time.Time) authRefreshAttempt {
	saved, applied, err := c.refreshStore.CompleteAuthConnectionRefresh(ctx, claim.Connection.ID, claim.LeaseToken, updated, refreshedAt)
	if err != nil {
		return authRefreshAttempt{result: transientRefreshResult("refresh_completion_failed"), err: err}
	}
	if !applied || saved == nil {
		return c.resultAfterStaleClaim(ctx, claim.Connection.ID)
	}
	return authRefreshAttempt{connection: saved, result: AuthRefreshResult{Outcome: AuthRefreshOutcomeRefreshed}}
}

// resultAfterStaleClaim reloads the winner's durable decision rather than
// guessing whether a lost lease refreshed or invalidated the grant.
func (c *AuthRefreshCoordinator) resultAfterStaleClaim(ctx context.Context, connectionID uuid.UUID) authRefreshAttempt {
	connection, err := c.refreshStore.GetAuthConnectionByID(ctx, connectionID)
	if err != nil || connection == nil {
		return authRefreshAttempt{result: transientRefreshResult("refresh_claim_lost"), err: ErrAuthRefreshFailed}
	}
	if connection.RefreshState == reconnectRequiredCode {
		return authRefreshAttempt{connection: connection, result: AuthRefreshResult{Outcome: AuthRefreshOutcomeReconnectRequired, FailureCode: stableReconnectFailureCode(connection)}}
	}
	if !authConnectionNeedsRefresh(connection, c.now().UTC()) {
		return authRefreshAttempt{connection: connection, result: AuthRefreshResult{Outcome: AuthRefreshOutcomeLeaseContended, FailureCode: "refresh_claim_lost"}}
	}
	return authRefreshAttempt{connection: connection, result: transientRefreshResult("refresh_claim_lost"), err: ErrAuthRefreshFailed}
}

// waitForForegroundRefresh boundedly observes another owner's CAS result and
// returns the previous token only while its provider expiry remains in future.
func (c *AuthRefreshCoordinator) waitForForegroundRefresh(ctx context.Context, original *store.AuthConnection) (*store.AuthConnection, error) {
	// Why: the coordination bound uses monotonic wall time rather than the
	// injectable provider clock, which may intentionally stay fixed in tests.
	deadline := time.Now().Add(c.foregroundWait)
	for time.Now().Before(deadline) {
		connection, done, err := c.reloadForegroundRefresh(ctx, original)
		if err != nil {
			return foregroundTransientResult(original, c.now().UTC(), ErrAuthRefreshFailed)
		}
		if done {
			return connection, foregroundConnectionStateError(connection)
		}
		if err := waitForAuthRefreshPoll(ctx, c.foregroundPollInterval); err != nil {
			return nil, err
		}
	}
	connection, _, err := c.reloadForegroundRefresh(ctx, original)
	if err == nil && connection != nil {
		return foregroundTransientResult(connection, c.now().UTC(), ErrAuthRefreshFailed)
	}
	return foregroundTransientResult(original, c.now().UTC(), ErrAuthRefreshFailed)
}

// reloadForegroundRefresh recognizes either a successful rotation or a durable
// reconnect decision while treating an unchanged due connection as in flight.
func (c *AuthRefreshCoordinator) reloadForegroundRefresh(ctx context.Context, original *store.AuthConnection) (*store.AuthConnection, bool, error) {
	connection, err := c.refreshStore.GetAuthConnectionByID(ctx, original.ID)
	if err != nil || connection == nil {
		return nil, false, err
	}
	if connection.RefreshState == reconnectRequiredCode {
		return connection, true, nil
	}
	return connection, !authConnectionNeedsRefresh(connection, c.now().UTC()), nil
}

// waitForAuthRefreshPoll makes foreground lease contention cancellation-aware.
func waitForAuthRefreshPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// foregroundAttemptResult maps bounded coordinator decisions into SDK behavior
// while allowing a still-valid access token through after transient failures.
func (c *AuthRefreshCoordinator) foregroundAttemptResult(original *store.AuthConnection, now time.Time, attempt authRefreshAttempt) (*store.AuthConnection, error) {
	switch attempt.result.Outcome {
	case AuthRefreshOutcomeRefreshed:
		return attempt.connection, nil
	case AuthRefreshOutcomeReconnectRequired:
		connection := attempt.connection
		if connection == nil {
			connection = original
		}
		return nil, newReconnectRequiredError(connection, attempt.result.FailureCode)
	case AuthRefreshOutcomeLeaseContended:
		if attempt.connection != nil && !authConnectionNeedsRefresh(attempt.connection, now) {
			return attempt.connection, nil
		}
		return foregroundTransientResult(original, now, sanitizedAuthRefreshError(attempt))
	default:
		return foregroundTransientResult(original, now, sanitizedAuthRefreshError(attempt))
	}
}

// authConnectionRefreshRetryScheduled prevents request traffic from bypassing
// the persisted backoff chosen by the last transient refresh attempt.
func authConnectionRefreshRetryScheduled(conn *store.AuthConnection, now time.Time) bool {
	return conn.RefreshRetryNotBefore != nil && conn.RefreshRetryNotBefore.After(now)
}

// authConnectionRefreshAttemptActive uses the durable claim timestamp as the
// safe signal that a nil exact claim represents live cross-request contention.
func authConnectionRefreshAttemptActive(conn *store.AuthConnection, now time.Time) bool {
	if conn.LastRefreshAttemptAt == nil || conn.LastRefreshAttemptAt.After(now) {
		return false
	}
	return conn.LastRefreshAttemptAt.Add(defaultAuthRefreshAttemptActiveWindow).After(now)
}

// foregroundTransientResult preserves availability before access expiry and
// fails closed once the provider credential is no longer valid.
func foregroundTransientResult(conn *store.AuthConnection, now time.Time, refreshErr error) (*store.AuthConnection, error) {
	if !authConnectionExpired(conn, now) {
		return conn, nil
	}
	if refreshErr == nil {
		refreshErr = ErrAuthRefreshFailed
	}
	return nil, refreshErr
}

// foregroundConnectionStateError converts a durable reconnect state into the
// existing typed SDK action without exposing stored operational details.
func foregroundConnectionStateError(conn *store.AuthConnection) error {
	if conn != nil && conn.RefreshState == reconnectRequiredCode {
		return newReconnectRequiredError(conn, stableReconnectFailureCode(conn))
	}
	return nil
}

// stableReconnectFailureCode selects only persisted Engine decision codes for
// the SDK action and supplies a safe fallback for legacy rows.
func stableReconnectFailureCode(conn *store.AuthConnection) string {
	if conn != nil && strings.TrimSpace(conn.LastFailureCode) != "" {
		return conn.LastFailureCode
	}
	return "stored_grant_unusable"
}

// transientRefreshResult builds the common bounded retryable result.
func transientRefreshResult(failureCode string) AuthRefreshResult {
	return AuthRefreshResult{Outcome: AuthRefreshOutcomeTransientFailure, FailureCode: failureCode}
}

// sanitizedAuthRefreshError prevents worker logs from receiving provider or
// persistence error text while retaining a stable operational classification.
func sanitizedAuthRefreshError(attempt authRefreshAttempt) error {
	if attempt.err == nil {
		return nil
	}
	if attempt.result.Outcome == AuthRefreshOutcomeContractUnavailable {
		return ErrAuthRefreshContractUnavailable
	}
	return ErrAuthRefreshFailed
}

// connectionFromRefreshToken reuses the connection DEK and identity while
// replacing only provider token material returned by the refresh grant.
func (c *AuthRefreshCoordinator) connectionFromRefreshToken(conn *store.AuthConnection, token connectauth.TokenResponse, previousRefreshToken string, now time.Time) (store.AuthConnection, error) {
	dek, err := store.UnwrapDEK(c.masterKey, conn.EncryptedDEK)
	if err != nil {
		return store.AuthConnection{}, err
	}
	access, err := store.EncryptWithDEK(dek, token.AccessToken)
	if err != nil {
		return store.AuthConnection{}, err
	}
	refresh, err := refreshedOptionalToken(dek, token.RefreshToken, conn.EncryptedRefreshToken)
	if err != nil {
		return store.AuthConnection{}, err
	}
	idToken, err := refreshedOptionalToken(dek, token.IDToken, conn.EncryptedIDToken)
	if err != nil {
		return store.AuthConnection{}, err
	}
	updated := refreshedConnectionMetadata(conn, token, previousRefreshToken, now)
	updated.EncryptedAccessToken = access
	updated.EncryptedRefreshToken = refresh
	updated.EncryptedIDToken = idToken
	return updated, nil
}

// refreshedConnectionMetadata applies provider metadata while preserving fields
// omitted from refresh responses according to OAuth/OIDC conventions.
func refreshedConnectionMetadata(conn *store.AuthConnection, token connectauth.TokenResponse, previousRefreshToken string, now time.Time) store.AuthConnection {
	claims := refreshIdentityClaims(token, conn)
	updated := *conn
	updated.TokenType = connectauth.DefaultTokenType(token.TokenType)
	scopeSet := connectauth.TokenScopeMetadata(token, conn.Scopes, conn.ScopeSource)
	updated.Scopes = scopeSet.Scopes
	updated.ScopeSource = scopeSet.Source
	updated.Issuer = connectauth.ClaimString(claims, "iss")
	updated.Subject = connectauth.ClaimString(claims, "sub")
	updated.IdentityClaims = connectauth.ClaimBytes(claims)
	updated.ExpiresAt = authRefreshExpiry(now, token.ExpiresIn)
	if refreshExpiresAt := authRefreshExpiry(now, token.RefreshTokenExpiresIn); refreshExpiresAt != nil {
		updated.RefreshTokenExpiresAt = refreshExpiresAt
	} else if strings.TrimSpace(token.RefreshToken) != "" && token.RefreshToken != previousRefreshToken {
		// Why: an absolute expiry belongs to the previous refresh token; a
		// rotated token without TTL metadata must not inherit that stale deadline.
		updated.RefreshTokenExpiresAt = nil
	}
	updated.RefreshState = "ok"
	updated.LastFailureCode = ""
	updated.LastFailureAt = nil
	updated.LastFailureTraceID = ""
	updated.RefreshRetryNotBefore = nextSuccessfulRefreshEligibility(now, updated.ExpiresAt, updated.RefreshTokenExpiresAt)
	return updated
}

// nextSuccessfulRefreshEligibility prevents staggered Engine replicas from
// rotating a newly refreshed token again while retaining the expiry margin.
func nextSuccessfulRefreshEligibility(now time.Time, expiries ...*time.Time) *time.Time {
	next := now.Add(defaultAuthRefreshSuccessInterval)
	for _, expiry := range expiries {
		if expiry == nil {
			continue
		}
		candidate := expiry.Add(-defaultAuthRefreshExpirySafetyMargin)
		if candidate.Before(next) {
			next = candidate
		}
	}
	minimum := now.Add(defaultAuthRefreshMinimumCooldown)
	if next.Before(minimum) {
		// Why: very short provider TTLs must not create a cross-replica hot loop.
		next = minimum
	}
	return &next
}

// authRefreshExpiry derives deterministic provider expiry from the attempt clock.
func authRefreshExpiry(now time.Time, expiresIn int64) *time.Time {
	if expiresIn <= 0 {
		return nil
	}
	expiresAt := now.Add(time.Duration(expiresIn) * time.Second)
	return &expiresAt
}

// refreshIdentityClaims preserves prior OIDC metadata when refresh omits id_token.
func refreshIdentityClaims(token connectauth.TokenResponse, conn *store.AuthConnection) map[string]any {
	if strings.TrimSpace(token.IDToken) != "" {
		return connectauth.OIDCClaims(token.IDToken)
	}
	var claims map[string]any
	if len(conn.IdentityClaims) == 0 {
		return nil
	}
	if err := json.Unmarshal(conn.IdentityClaims, &claims); err != nil {
		return nil
	}
	return claims
}

// connectedRefreshAuth selects the named OAuth/OIDC definition used by dispatch.
func connectedRefreshAuth(authName string, auths fusedobject.AuthConfigs) (fusedobject.AuthConfig, fusedobject.OAuth2FlowContract, error) {
	for _, auth := range auths {
		if authCredentialName(auth) == authName && isRefreshableAuth(auth) {
			return validateRefreshableAuth(auth)
		}
	}
	return fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, errors.New("refreshable auth config not found")
}

// validateRefreshableAuth fails before network I/O without a token endpoint.
func validateRefreshableAuth(auth fusedobject.AuthConfig) (fusedobject.AuthConfig, fusedobject.OAuth2FlowContract, error) {
	flow, ok := auth.OAuth2Flows["authorizationCode"]
	if !ok || strings.TrimSpace(flow.TokenURL) == "" {
		return fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, errors.New("refresh authorizationCode flow requires token_url")
	}
	return auth, flow, nil
}

// isRefreshableAuth limits rotation to Engine-owned OAuth/OIDC connections.
func isRefreshableAuth(auth fusedobject.AuthConfig) bool {
	return auth.Type == "oauth2" || auth.Type == "openIdConnect" || auth.Type == "oidc"
}

// authConnectionNeedsRefresh identifies known expiries inside the request window.
func authConnectionNeedsRefresh(conn *store.AuthConnection, now time.Time) bool {
	dueBy := now.Add(connectedAuthRefreshWindow)
	accessDue := conn.ExpiresAt != nil && !conn.ExpiresAt.After(dueBy)
	refreshDue := strings.TrimSpace(conn.EncryptedRefreshToken) != "" && conn.RefreshTokenExpiresAt != nil && !conn.RefreshTokenExpiresAt.After(dueBy)
	return accessDue || refreshDue
}

// authConnectionExpired distinguishes failed proactive refresh from unsafe dispatch.
func authConnectionExpired(conn *store.AuthConnection, now time.Time) bool {
	return conn.ExpiresAt != nil && !conn.ExpiresAt.After(now)
}

// authConnectionAccessOnlyAndValid preserves a provider-issued access token
// until expiry when no refresh grant exists to rotate proactively.
func authConnectionAccessOnlyAndValid(conn *store.AuthConnection, now time.Time) bool {
	return strings.TrimSpace(conn.EncryptedRefreshToken) == "" && !authConnectionExpired(conn, now)
}

// authConnectionRefreshTokenExpired trusts only provider-declared refresh TTL.
func authConnectionRefreshTokenExpired(conn *store.AuthConnection, now time.Time) bool {
	return conn.RefreshTokenExpiresAt != nil && !conn.RefreshTokenExpiresAt.After(now)
}

// refreshedOptionalToken preserves existing encrypted material on provider omission.
func refreshedOptionalToken(dek []byte, value, fallbackEncrypted string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return fallbackEncrypted, nil
	}
	return store.EncryptWithDEK(dek, value)
}

// decryptAuthConnectionToken decrypts the explicitly selected token ciphertext.
func decryptAuthConnectionToken(masterKey []byte, encryptedDEK, encryptedValue string) (string, error) {
	dek, err := store.UnwrapDEK(masterKey, encryptedDEK)
	if err != nil {
		return "", err
	}
	return store.DecryptWithDEK(dek, encryptedValue)
}
