package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrManagedLoginPending     = errors.New("managed login is pending")
	ErrManagedLoginUnavailable = errors.New("managed login is unavailable")
	ErrManagedIdentityDenied   = errors.New("managed identity is not invited")
)

const (
	ManagedLoginStatePending    = "pending"
	ManagedLoginStateExchanging = "exchanging"
	ManagedLoginStateVerified   = "verified"
)

type ManagedLoginTransaction struct {
	ID                        uuid.UUID
	RegistryTransactionID     uuid.UUID
	AccountID                 uuid.UUID
	InstallationID            uuid.UUID
	Purpose                   string
	PollSecretHash            string
	EnrollmentRef             string
	EncryptedDEK              string
	EncryptedRegistryVerifier string
	State                     string
	Provider                  string
	Issuer                    string
	ExternalSubject           string
	VerifiedEmail             string
	DisplayName               string
	AuthMethod                string
	AuthenticatedAt           time.Time
	ExpiresAt                 time.Time
}

type VerifiedManagedIdentity struct {
	AccountID        uuid.UUID
	InstallationID   uuid.UUID
	Purpose          string
	Provider         string
	Issuer           string
	ExternalSubject  string
	VerifiedEmail    string
	DisplayName      string
	AuthMethod       string
	EnrollmentRef    string
	AuthenticatedAt  time.Time
	AssertionExpires time.Time
}

type ManagedSessionCredential struct {
	UserID                uuid.UUID
	CredentialID          uuid.UUID
	RawKey                string
	ExpiresAt             time.Time
	AuthorizationRevision int64
	AuthMethod            string
}

type ManagedIdentityStore interface {
	CreateManagedLoginTransaction(context.Context, ManagedLoginTransaction) error
	ClaimManagedLoginExchange(context.Context, uuid.UUID, string, time.Time) (ManagedLoginTransaction, error)
	ReleaseManagedLoginExchange(context.Context, uuid.UUID, string, time.Time) error
	RejectManagedLoginTransaction(context.Context, uuid.UUID, string, time.Time) error
	SaveManagedLoginAssertion(context.Context, uuid.UUID, string, VerifiedManagedIdentity, time.Time) error
	ConsumeManagedLoginAssertion(context.Context, uuid.UUID, string, time.Time, time.Time) (ManagedSessionCredential, error)
	ExpireManagedLoginTransactions(context.Context, time.Time, int) (int64, error)
}
