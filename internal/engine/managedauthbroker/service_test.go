package managedauthbroker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeTicketVerifier struct {
	accountID      uuid.UUID
	installationID uuid.UUID
	err            error
}

// VerifyTicket supplies a bounded Registry identity without transport dependencies.
func (f fakeTicketVerifier) VerifyTicket(context.Context, string) (EnrollmentIdentity, error) {
	return EnrollmentIdentity{AccountID: f.accountID, InstallationID: f.installationID, ExpiresAt: time.Now().Add(5 * time.Minute)}, f.err
}

func TestEnrollRejectsInvalidTicketWithoutTouchingStore(t *testing.T) {
	// nil store: a rejected ticket must return before any store call, so this
	// panicking on a nil pointer dereference would itself fail the test.
	service, err := NewService(&Store{}, fakeTicketVerifier{err: errors.New("registry says no")})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := service.Enroll(context.Background(), "bad-ticket"); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("expected ErrInvalidTicket, got %v", err)
	}
}

func TestNewServiceRejectsMissingDependencies(t *testing.T) {
	if _, err := NewService(nil, fakeTicketVerifier{}); err == nil {
		t.Fatal("expected error for nil store")
	}
	if _, err := NewService(&Store{}, nil); err == nil {
		t.Fatal("expected error for nil verifier")
	}
}
