package store

import (
	"context"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

type BrowserSessionCredential struct {
	SubjectID             uuid.UUID
	CredentialID          uuid.UUID
	RawKey                string
	ExpiresAt             time.Time
	AuthorizationRevision int64
}

type BrowserLogoutContext struct {
	AuthorizationRevision int64
	EncryptedDEK          string
	EncryptedLogoutToken  string
	ExpiresAt             time.Time
}

type BrowserSessionStore interface {
	IssueBrowserSession(context.Context, accesscontrol.Actor, string, time.Time) (BrowserSessionCredential, error)
	RevokeBrowserSession(context.Context, accesscontrol.Actor, time.Time) (BrowserLogoutContext, error)
}
