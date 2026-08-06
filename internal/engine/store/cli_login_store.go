package store

import (
	"context"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

var (
	ErrCLILoginPending     = errors.New("CLI login is pending")
	ErrCLILoginDenied      = errors.New("CLI login is denied")
	ErrCLILoginUnavailable = errors.New("CLI login is unavailable")
	ErrCLILogoutDenied     = errors.New("CLI logout is denied")
)

const (
	CLILoginStatePending  = "pending"
	CLILoginStateApproved = "approved"
	CLILoginStateConsumed = "consumed"
)

type CLILoginTransaction struct {
	ID                  uuid.UUID
	PollSecretHash      string
	BrowserSecretHash   string
	CredentialHash      string
	CredentialPrefix    string
	ExpiresAt           time.Time
	CredentialExpiresAt time.Time
}

type CLILoginCredential struct {
	SubjectID             uuid.UUID
	CredentialID          uuid.UUID
	ExpiresAt             time.Time
	AuthorizationRevision int64
}

type CLILogoutResult struct {
	AuthorizationRevision int64
}

type CLILoginStore interface {
	CreateCLILoginTransaction(context.Context, CLILoginTransaction) error
	ApproveCLILoginTransaction(context.Context, uuid.UUID, string, accesscontrol.Actor, time.Time) error
	ConsumeCLILoginTransaction(context.Context, uuid.UUID, string, time.Time) (CLILoginCredential, error)
	RevokeCurrentCLICredential(context.Context, MutationActor) (CLILogoutResult, error)
}
