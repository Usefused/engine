package cliauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const (
	defaultTransactionTTL = 10 * time.Minute
	defaultCredentialTTL  = 30 * 24 * time.Hour
)

type RevisionSink interface {
	SetRevision(int64) bool
}

type StartInput struct {
	CredentialHash   string
	CredentialPrefix string
}

type StartResult struct {
	TransactionID uuid.UUID
	PollToken     string
	BrowserToken  string
	ExpiresAt     time.Time
}

type Service struct {
	store          store.CLILoginStore
	revisions      RevisionSink
	now            func() time.Time
	transactionTTL time.Duration
	credentialTTL  time.Duration
}

func NewService(repository store.CLILoginStore, revisions RevisionSink) (*Service, error) {
	if repository == nil || revisions == nil {
		return nil, errors.New("invalid CLI authentication configuration")
	}
	return &Service{
		store: repository, revisions: revisions, now: time.Now,
		transactionTTL: defaultTransactionTTL, credentialTTL: defaultCredentialTTL,
	}, nil
}

func (s *Service) Start(ctx context.Context, input StartInput) (StartResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.cli.start")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"), attribute.String("client.type", "cli"))
	if !validCredentialCommitment(input) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return StartResult{}, store.ErrCLILoginDenied
	}
	result, transaction, err := s.prepareTransaction(input)
	if err == nil {
		err = s.store.CreateCLILoginTransaction(ctx, transaction)
	}
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return StartResult{}, err
	}
	span.SetAttributes(attribute.String("outcome", "started"))
	return result, nil
}

func (s *Service) Approve(ctx context.Context, id uuid.UUID, browserToken string, actor accesscontrol.Actor) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.cli.approve")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"), attribute.String("client.type", "cli"))
	if id == uuid.Nil || !validCapability(browserToken) || !browserauth.IsBrowserSessionActor(actor) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return store.ErrCLILoginDenied
	}
	err := s.store.ApproveCLILoginTransaction(ctx, id, hashSecret(browserToken), actor, s.now().UTC())
	if err != nil {
		span.SetAttributes(attribute.String("outcome", cliLoginOutcome(err)))
		return err
	}
	span.SetAttributes(attribute.String("outcome", "approved"), attribute.String("identity.auth_method", actor.AuthenticationMethod))
	return nil
}

func (s *Service) Poll(ctx context.Context, id uuid.UUID, pollToken string) (store.CLILoginCredential, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.identity.cli.consume")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "user"), attribute.String("client.type", "cli"))
	if id == uuid.Nil || !validCapability(pollToken) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return store.CLILoginCredential{}, store.ErrCLILoginDenied
	}
	credential, err := s.store.ConsumeCLILoginTransaction(ctx, id, hashSecret(pollToken), s.now().UTC())
	if err != nil {
		span.SetAttributes(attribute.String("outcome", cliLoginOutcome(err)))
		return store.CLILoginCredential{}, err
	}
	s.revisions.SetRevision(credential.AuthorizationRevision)
	span.SetAttributes(attribute.String("outcome", "authenticated"))
	return credential, nil
}

func (s *Service) prepareTransaction(input StartInput) (StartResult, store.CLILoginTransaction, error) {
	pollToken, err := randomToken()
	if err != nil {
		return StartResult{}, store.CLILoginTransaction{}, err
	}
	browserToken, err := randomToken()
	if err != nil {
		return StartResult{}, store.CLILoginTransaction{}, err
	}
	now := s.now().UTC()
	result := StartResult{
		TransactionID: uuid.New(), PollToken: pollToken, BrowserToken: browserToken,
		ExpiresAt: now.Add(s.transactionTTL),
	}
	transaction := store.CLILoginTransaction{
		ID: result.TransactionID, PollSecretHash: hashSecret(pollToken),
		BrowserSecretHash: hashSecret(browserToken), CredentialHash: input.CredentialHash,
		CredentialPrefix: input.CredentialPrefix, ExpiresAt: result.ExpiresAt,
		CredentialExpiresAt: now.Add(s.credentialTTL),
	}
	return result, transaction, nil
}

func validCredentialCommitment(input StartInput) bool {
	if len(input.CredentialHash) != sha256.Size*2 || len(input.CredentialPrefix) != 8 {
		return false
	}
	if input.CredentialPrefix[:4] != "fsk_" {
		return false
	}
	decoded, err := hex.DecodeString(input.CredentialHash)
	return err == nil && hex.EncodeToString(decoded) == input.CredentialHash
}

func validCapability(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func hashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func cliLoginOutcome(err error) string {
	switch {
	case errors.Is(err, store.ErrCLILoginPending):
		return "pending"
	case errors.Is(err, store.ErrCLILoginDenied):
		return "denied"
	default:
		return "failed"
	}
}
