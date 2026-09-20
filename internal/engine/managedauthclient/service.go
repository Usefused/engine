package managedauthclient

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Status reports saved disable or the current readiness of an enabled installation.
type Status string

const (
	StatusDisabled           Status = "disabled"
	StatusReady              Status = "ready"
	StatusEnrollmentRequired Status = "enrollment_required"
	StatusTemporarilyDown    Status = "temporarily_unavailable"
)

// accessRefreshMargin refreshes an access token proactively rather than
// waiting for it to fail on a live connect attempt.
const accessRefreshMargin = time.Minute

// refreshTokenAssumedTTL is a local, soft estimate used only for enrollment status; the broker's own response is always the actual
// authority (a 401 from Refresh triggers re-enrollment regardless of this).
const refreshTokenAssumedTTL = 29 * 24 * time.Hour

type TicketMinter interface {
	MintManagedAuthEnrollmentTicket(ctx context.Context) (ticket string, err error)
}

type BrokerClient interface {
	Enroll(ctx context.Context, ticket string) (accessToken, refreshToken string, expiresIn int64, err error)
	Refresh(ctx context.Context, priorRefreshToken, ticket string) (accessToken, refreshToken string, expiresIn int64, err error)
	Revoke(ctx context.Context, refreshToken string) error
}

type Service struct {
	store     *Store
	minter    TicketMinter
	broker    BrokerClient
	masterKey []byte
	now       func() time.Time
}

func NewService(store *Store, minter TicketMinter, broker BrokerClient, masterKey []byte) (*Service, error) {
	if store == nil || minter == nil || broker == nil || len(masterKey) != 32 {
		return nil, errors.New("invalid managed-auth client configuration")
	}
	return &Service{store: store, minter: minter, broker: broker, masterKey: masterKey, now: time.Now}, nil
}

// Enroll enables or repairs enrollment while preserving a healthy credential across restarts and replicas.
func (s *Service) Enroll(ctx context.Context) error {
	// Save opt-in before attempting a network exchange so an outage remains retryable.
	if err := s.store.SetEnabled(ctx, true); err != nil {
		return err
	}
	return s.reconcile(ctx, true)
}

// ErrDisabled blocks delegated OAuth without disturbing locally owned provider connections.
var ErrDisabled = errors.New("Fused Managed Auth is disabled")

// Disable commits opt-out before attempting remote revocation, which the worker retries after outages.
func (s *Service) Disable(ctx context.Context) error {
	// Disable must survive even if the broker or Registry cannot be reached.
	if err := s.store.SetEnabled(ctx, false); err != nil {
		return err
	}
	// Remaining encrypted credentials are durable pending-revocation work; the status surface reports them.
	_ = s.reconcile(ctx, false)
	return nil
}

// revoke removes the local credential only after the broker acknowledges idempotent revocation.
func (s *Service) revoke(ctx context.Context) error {
	cred, err := s.store.Get(ctx, s.masterKey)
	// An installation that never enrolled has no remote credential to revoke.
	if errors.Is(err, ErrNotEnrolled) {
		return nil
	}
	// Preserve pending work when decryption or persistence is temporarily unavailable.
	if err != nil {
		return err
	}
	// Retry transport failures without reversing the saved opt-out.
	if err := s.broker.Revoke(ctx, cred.RefreshToken); err != nil {
		return err
	}
	return s.store.clear(ctx)
}

// enroll obtains fresh Registry authority when no reusable broker credential remains.
// The caller holds the database reconciliation lock until encrypted persistence commits.
func (s *Service) enroll(ctx context.Context) error {
	ticket, err := s.minter.MintManagedAuthEnrollmentTicket(ctx)
	// An unavailable or rejecting Registry cannot silently reactivate an installation.
	if err != nil {
		return err
	}
	accessToken, refreshToken, expiresIn, err := s.broker.Enroll(ctx, ticket)
	// Do not replace local state with a failed remote response.
	if err != nil {
		return err
	}
	return s.persist(ctx, accessToken, refreshToken, expiresIn)
}

// persist validates the complete pair before storing it with the ordinary Engine envelope encryption.
func (s *Service) persist(ctx context.Context, accessToken, refreshToken string, expiresIn int64) error {
	// Empty or nonsensical credentials must not become a successful local enrollment.
	if accessToken == "" || refreshToken == "" || expiresIn <= 0 || expiresIn > 86400 {
		return errors.New("managed-auth broker returned invalid credentials")
	}
	now := s.now()
	accessExpiresAt := now.Add(time.Duration(expiresIn) * time.Second)
	return s.store.Save(ctx, accessToken, refreshToken, accessExpiresAt, now.Add(refreshTokenAssumedTTL), s.masterKey)
}

// EnsureFresh reconciles an existing enrollment before any caller uses its broker token.
// Initial enrollment still requires the startup capability or the explicit enable action.
func (s *Service) EnsureFresh(ctx context.Context) error {
	enabled, _, err := s.store.State(ctx)
	// Never make a delegated request when saved intent is unavailable.
	if err != nil {
		return err
	}
	// Disabled callers fail locally; the background worker handles pending revocation.
	if !enabled {
		return ErrDisabled
	}
	return s.reconcile(ctx, false)
}

// reconcile makes decisions only after acquiring the installation-wide credential lock.
func (s *Service) reconcile(ctx context.Context, allowInitialEnrollment bool) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.managedauthclient.reconcile")
	defer span.End()
	disabled := false
	err := s.store.reconcile(ctx, func(ctx context.Context, locked *Store) error {
		// Bind persistence to the lock-owning transaction without mutating the shared service.
		scoped := *s
		scoped.store = locked
		enabled, _, err := locked.State(ctx)
		// Unknown intent cannot authorize delegation.
		if err != nil {
			return err
		}
		disabled = !enabled
		// Reconciliation retries revocation while preserving disabled intent across restarts.
		if disabled {
			return scoped.revoke(ctx)
		}
		return scoped.ensureFresh(ctx, allowInitialEnrollment)
	})
	span.SetAttributes(attribute.String("outcome", outcomeOf(err)))
	// Report disabled only after committing successful revocation cleanup.
	if err == nil && disabled {
		return ErrDisabled
	}
	return err
}

// ensureFresh reuses live credentials and recovers uncertain remote rotations through fresh Registry authority.
func (s *Service) ensureFresh(ctx context.Context, allowInitialEnrollment bool) error {
	cred, err := s.store.Get(ctx, s.masterKey)
	// Startup/enable retries initial failures; ordinary reads do not invent an enrollment.
	if errors.Is(err, ErrNotEnrolled) && allowInitialEnrollment {
		return s.enroll(ctx)
	}
	// Storage and decryption failures must not trigger a destructive replacement.
	if err != nil {
		return err
	}
	// Replica starts and repeated enable requests reuse the durable pair.
	if s.now().Add(accessRefreshMargin).Before(cred.AccessExpiresAt) {
		return nil
	}
	return s.refresh(ctx, cred)
}

// refresh preserves local state on transient errors and re-enrolls only after explicit refresh rejection.
func (s *Service) refresh(ctx context.Context, cred Credential) error {
	ticket, err := s.minter.MintManagedAuthEnrollmentTicket(ctx)
	// Renewals require current Registry approval; possession of a long-lived refresh token is insufficient.
	if err != nil {
		return err
	}
	accessToken, refreshToken, expiresIn, err := s.broker.Refresh(ctx, cred.RefreshToken, ticket)
	var rejected ErrBrokerRejected
	// A lost rotation response or failed local commit can leave a consumed refresh token; fresh Registry proof repairs it.
	if errors.As(err, &rejected) && rejected.StatusCode == 401 {
		return s.enroll(ctx)
	}
	// Server outages, throttles, and authorization policy failures never erase the last credential.
	if err != nil {
		return err
	}
	return s.persist(ctx, accessToken, refreshToken, expiresIn)
}

// outcomeOf reports a bounded diagnostic instead of including bearer material.
func outcomeOf(err error) string {
	if err != nil {
		return "failed"
	}
	return "succeeded"
}

// AccessToken returns a currently-valid broker access token, refreshing
// first if needed.
func (s *Service) AccessToken(ctx context.Context) (string, error) {
	if err := s.EnsureFresh(ctx); err != nil {
		return "", err
	}
	cred, err := s.store.Get(ctx, s.masterKey)
	if err != nil {
		return "", err
	}
	return cred.AccessToken, nil
}

// Status reports the coarse enrollment state without forcing a refresh, so
// a status page read never has side effects.
func (s *Service) Status(ctx context.Context) Status {
	enabled, _, stateErr := s.store.State(ctx)
	// Status must not suggest readiness when saved intent cannot be read.
	if stateErr != nil {
		return StatusTemporarilyDown
	}
	// Opt-out wins over the presence of credentials retained solely for revocation retry.
	if !enabled {
		return StatusDisabled
	}
	cred, err := s.store.Get(ctx, s.masterKey)
	if errors.Is(err, ErrNotEnrolled) {
		return StatusEnrollmentRequired
	}
	if err != nil {
		return StatusTemporarilyDown
	}
	// A locally expired refresh estimate requires renewed enrollment authority.
	if !s.now().Before(cred.RefreshExpiresAt) {
		return StatusEnrollmentRequired
	}
	// Expired access credentials cannot truthfully report readiness before reconciliation succeeds.
	if !s.now().Before(cred.AccessExpiresAt) {
		return StatusTemporarilyDown
	}
	return StatusReady
}

// StartRefreshWorker reconciles immediately and periodically, retrying initial enrollment after outages.
// It runs only when Registry advertises the managed-auth capability.
func (s *Service) StartRefreshWorker(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			// Reconciliation is bounded and idempotent, so startup failures can be retried without replacing healthy credentials.
			if err := s.reconcile(ctx, true); err != nil && !errors.Is(err, ErrDisabled) && ctx.Err() == nil {
				slog.WarnContext(ctx, "managed-auth broker enrollment reconciliation failed", slog.String("error_code", "managed_auth_reconciliation_failed"))
			}
			// Shutdown interrupts both idle waiting and the current reconciliation context.
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
