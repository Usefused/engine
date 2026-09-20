package managedauthbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/managedauthtransport"

	"github.com/google/uuid"
)

// RegistryTicketVerifier implements TicketVerifier by redeeming a ticket
// against Registry's own introspection endpoint. Registry, not this Engine,
// is the authority on whether the ticket's issuing account is legitimate --
// this call never inspects or trusts anything about the caller beyond the
// ticket string itself.
type RegistryTicketVerifier struct {
	baseURL    string
	httpClient *http.Client
}

// NewRegistryTicketVerifier derives Registry's REST base URL from the same
// GraphQL endpoint Engine already uses for every other Registry call
// (registryBaseURL in sandbox.HTTPRegistryClient strips the same suffix).
func NewRegistryTicketVerifier(registryGraphQLEndpoint string, httpClient *http.Client) *RegistryTicketVerifier {
	httpClient = managedauthtransport.New(httpClient, 10*time.Second)
	base := strings.TrimSuffix(strings.TrimRight(registryGraphQLEndpoint, "/"), "/graphql")
	return &RegistryTicketVerifier{baseURL: base, httpClient: httpClient}
}

// VerifyTicket accepts only a complete installation identity returned by the trusted Registry.
func (v *RegistryTicketVerifier) VerifyTicket(ctx context.Context, ticket string) (EnrollmentIdentity, error) {
	body, err := json.Marshal(struct {
		Ticket string `json:"ticket"`
	}{Ticket: ticket})
	// Fail closed when the trusted Registry exchange cannot complete.
	if err != nil {
		return EnrollmentIdentity{}, fmt.Errorf("encode managed-auth ticket introspection request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.baseURL+"/api/engine/managed-auth/enrollment-ticket/introspect", bytes.NewReader(body))
	// Fail closed when the trusted Registry exchange cannot complete.
	if err != nil {
		return EnrollmentIdentity{}, fmt.Errorf("build managed-auth ticket introspection request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := v.httpClient.Do(request)
	// Fail closed when the trusted Registry exchange cannot complete.
	if err != nil {
		return EnrollmentIdentity{}, fmt.Errorf("managed-auth ticket introspection request failed: %w", err)
	}
	defer response.Body.Close()
	// Only successful redemption establishes an authenticated identity.
	if response.StatusCode != http.StatusOK {
		return EnrollmentIdentity{}, fmt.Errorf("%w: registry returned %d", ErrInvalidTicket, response.StatusCode)
	}
	var identity EnrollmentIdentity
	// Malformed responses are not a license to issue a partially scoped credential.
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&identity); err != nil {
		return EnrollmentIdentity{}, fmt.Errorf("decode managed-auth ticket introspection response: %w", err)
	}
	// Reject old account-only responses instead of inventing an installation identity.
	if identity.AccountID == uuid.Nil || identity.InstallationID == uuid.Nil {
		return EnrollmentIdentity{}, ErrInvalidTicket
	}
	return identity, nil
}
