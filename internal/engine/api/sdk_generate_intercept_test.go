package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
)

func TestIsSDKGeneratePath(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"POST /sdks/generate", http.MethodPost, "/sdks/generate", true},
		{"GET /sdks/generate", http.MethodGet, "/sdks/generate", false},
		{"POST /integrations", http.MethodPost, "/integrations", false},
		{"POST /sdks/generate/extra", http.MethodPost, "/sdks/generate/extra", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSDKGeneratePath(tt.method, tt.path); got != tt.want {
				t.Errorf("isSDKGeneratePath(%q, %q) = %v, want %v", tt.method, tt.path, got, tt.want)
			}
		})
	}
}

// sdkGenerateGateMockStore is a dedicated store.Store mock for Task 6's
// /sdks/generate workspace gate: it records IsWorkspaceServiceEnabled calls
// (so tests can assert the gate stops checking after the first miss isn't
// required, but batching isn't the point here -- correctness is) and lets
// tests configure per-service activation state directly.
type sdkGenerateGateMockStore struct {
	store.Store
	accountID       uuid.UUID
	workspaceID     uuid.UUID
	noWorkspace     bool
	activated       map[uuid.UUID]bool
	versions        map[uuid.UUID][]store.WorkspaceServiceVersion
	activationErr   error
	activationCalls []uuid.UUID
}

func (m *sdkGenerateGateMockStore) GetAccountByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, error) {
	return m.accountID, nil
}

func (m *sdkGenerateGateMockStore) VerifyWorkspaceOwner(ctx context.Context, accountID uuid.UUID) error {
	if m.noWorkspace {
		return errors.New("no workspace for account")
	}
	return nil
}

func (m *sdkGenerateGateMockStore) IsWorkspaceServiceEnabled(ctx context.Context, serviceID uuid.UUID) (bool, error) {
	m.activationCalls = append(m.activationCalls, serviceID)
	if m.activationErr != nil {
		return false, m.activationErr
	}
	return m.activated[serviceID], nil
}

func (m *sdkGenerateGateMockStore) ListWorkspaceServiceVersionsForServices(ctx context.Context, serviceIDs []uuid.UUID) (map[uuid.UUID][]store.WorkspaceServiceVersion, error) {
	out := map[uuid.UUID][]store.WorkspaceServiceVersion{}
	for _, serviceID := range serviceIDs {
		out[serviceID] = append([]store.WorkspaceServiceVersion(nil), m.versions[serviceID]...)
	}
	return out, nil
}

func TestFirstUnactivatedSelection_AllActivated_NotBlocked(t *testing.T) {
	svcA := uuid.New()
	svcB := uuid.New()
	versionA := uuid.New()
	versionB := uuid.New()
	s := &sdkGenerateGateMockStore{
		workspaceID: uuid.New(),
		activated:   map[uuid.UUID]bool{svcA: true, svcB: true},
		versions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			svcA: {{ServiceID: svcA, ServiceVersionID: versionA}},
			svcB: {{ServiceID: svcB, ServiceVersionID: versionB}},
		},
	}
	body := []byte(`{"selections":[{"service_id":"` + svcA.String() + `","service_version_id":"` + versionA.String() + `"},{"service_id":"` + svcB.String() + `","service_version_id":"` + versionB.String() + `"}]}`)

	_, blocked, err := firstUnactivatedSelection(context.Background(), s, uuid.New(), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked {
		t.Error("expected not blocked when every selection is activated")
	}
}

func TestFirstUnactivatedSelection_OneNotActivated_Blocked(t *testing.T) {
	svcA := uuid.New()
	svcB := uuid.New()
	versionA := uuid.New()
	versionB := uuid.New()
	s := &sdkGenerateGateMockStore{
		workspaceID: uuid.New(),
		activated:   map[uuid.UUID]bool{svcA: true, svcB: false},
		versions:    map[uuid.UUID][]store.WorkspaceServiceVersion{svcA: {{ServiceID: svcA, ServiceVersionID: versionA}}},
	}
	body := []byte(`{"selections":[{"service_id":"` + svcA.String() + `","service_version_id":"` + versionA.String() + `"},{"service_id":"` + svcB.String() + `","service_version_id":"` + versionB.String() + `"}]}`)

	blockedSelection, blocked, err := firstUnactivatedSelection(context.Background(), s, uuid.New(), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !blocked {
		t.Fatal("expected blocked when a selection isn't activated")
	}
	if blockedSelection.ServiceID != svcB {
		t.Errorf("expected blocked service %s, got %s", svcB, blockedSelection.ServiceID)
	}
}

func TestFirstUnactivatedSelection_MissingServiceVersionID_Blocked(t *testing.T) {
	svcA := uuid.New()
	s := &sdkGenerateGateMockStore{workspaceID: uuid.New(), activated: map[uuid.UUID]bool{svcA: true}}
	body := []byte(`{"selections":[{"service_id":"` + svcA.String() + `"}]}`)

	blockedSelection, blocked, err := firstUnactivatedSelection(context.Background(), s, uuid.New(), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !blocked || blockedSelection.BlockReason != "service_version_required" {
		t.Fatalf("expected service_version_required block, got blocked=%v selection=%#v", blocked, blockedSelection)
	}
}

func TestFirstUnactivatedSelection_DisabledServiceVersion_Blocked(t *testing.T) {
	svcA := uuid.New()
	enabledVersion := uuid.New()
	requestedVersion := uuid.New()
	s := &sdkGenerateGateMockStore{
		workspaceID: uuid.New(),
		activated:   map[uuid.UUID]bool{svcA: true},
		versions:    map[uuid.UUID][]store.WorkspaceServiceVersion{svcA: {{ServiceID: svcA, ServiceVersionID: enabledVersion}}},
	}
	body := []byte(`{"selections":[{"service_id":"` + svcA.String() + `","service_version_id":"` + requestedVersion.String() + `"}]}`)

	blockedSelection, blocked, err := firstUnactivatedSelection(context.Background(), s, uuid.New(), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !blocked || blockedSelection.BlockReason != "service_version_not_enabled" {
		t.Fatalf("expected service_version_not_enabled block, got blocked=%v selection=%#v", blocked, blockedSelection)
	}
}

// TestFirstUnactivatedSelection_NoWorkspace_BlocksFirstSelection covers an
// account that has never bootstrapped a workspace at all -- nothing could
// possibly be activated, so the request is blocked without needing to call
// IsWorkspaceServiceEnabled at all.
func TestFirstUnactivatedSelection_NoWorkspace_BlocksFirstSelection(t *testing.T) {
	svcA := uuid.New()
	s := &sdkGenerateGateMockStore{noWorkspace: true}
	body := []byte(`{"selections":[{"service_id":"` + svcA.String() + `"}]}`)

	blockedSelection, blocked, err := firstUnactivatedSelection(context.Background(), s, uuid.New(), body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !blocked || blockedSelection.ServiceID != svcA {
		t.Errorf("expected blocked=true with service %s, got blocked=%v id=%s", svcA, blocked, blockedSelection.ServiceID)
	}
	if len(s.activationCalls) != 0 {
		t.Errorf("expected no IsWorkspaceServiceEnabled calls when there's no workspace, got %d", len(s.activationCalls))
	}
}

func TestWorkspaceActivationRequiredMessage_UsesSlugForCommandHint(t *testing.T) {
	svcA := uuid.New()
	msg := workspaceActivationRequiredMessage(sdkGenerateSelection{
		ServiceID:   svcA,
		ServiceName: "Stripe Billing",
		ServiceSlug: "stripe-billing",
	})

	if !strings.Contains(msg, "service Stripe Billing is not activated") {
		t.Fatalf("expected message to name the service, got %q", msg)
	}
	if !strings.Contains(msg, "fused-cli workspace service add stripe-billing") {
		t.Fatalf("expected message to use the slug in the command hint, got %q", msg)
	}
}

func TestWorkspaceActivationRequiredMessage_QuotesNameFallback(t *testing.T) {
	msg := workspaceActivationRequiredMessage(sdkGenerateSelection{
		ServiceID:   uuid.New(),
		ServiceName: "Internal Billing API",
	})

	if !strings.Contains(msg, `fused-cli workspace service add "Internal Billing API"`) {
		t.Fatalf("expected command hint to quote a spaced service name, got %q", msg)
	}
}

// TestFirstUnactivatedSelection_MalformedJSON_NotBlockedNoError mirrors
// autoRegisterImportedService's own stance: a decode failure isn't this
// gate's concern to report -- it lets the request through so the Registry's
// own request validation produces the real error.
func TestFirstUnactivatedSelection_MalformedJSON_NotBlockedNoError(t *testing.T) {
	s := &sdkGenerateGateMockStore{workspaceID: uuid.New()}
	_, blocked, err := firstUnactivatedSelection(context.Background(), s, uuid.New(), []byte(`not json`))
	if err != nil {
		t.Fatalf("expected no error for malformed JSON, got %v", err)
	}
	if blocked {
		t.Error("expected malformed JSON to not be blocked here -- the Registry validates the body itself")
	}
}

func TestFirstUnactivatedSelection_EmptySelections_NotBlocked(t *testing.T) {
	s := &sdkGenerateGateMockStore{workspaceID: uuid.New()}
	_, blocked, err := firstUnactivatedSelection(context.Background(), s, uuid.New(), []byte(`{"selections":[]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked {
		t.Error("expected no selections to mean nothing to block")
	}
}

// TestFirstUnactivatedSelection_ActivationCheckError_ReturnsError is the
// fail-closed AC: unlike Task 3's best-effort auto-register (which swallows
// store errors since it's not a security control), this gate must surface an
// IsWorkspaceServiceEnabled failure as an error rather than silently treating it as
// either activated or not -- a security check that fails open on an internal
// error would defeat its own purpose.
func TestFirstUnactivatedSelection_ActivationCheckError_ReturnsError(t *testing.T) {
	svcA := uuid.New()
	versionA := uuid.New()
	s := &sdkGenerateGateMockStore{workspaceID: uuid.New(), activationErr: errors.New("db unavailable")}
	body := []byte(`{"selections":[{"service_id":"` + svcA.String() + `","service_version_id":"` + versionA.String() + `"}]}`)

	_, _, err := firstUnactivatedSelection(context.Background(), s, uuid.New(), body)
	if err == nil {
		t.Fatal("expected an error when IsWorkspaceServiceEnabled fails")
	}
}

// TestRESTProxyHandler_SDKGenerate_BlocksUnactivatedService and its sibling
// below are the end-to-end routing guards, mirroring
// TestRESTProxyHandler_ImportApply_RoutesThroughAutoRegister's style:
// POST /sdks/generate must run through the workspace gate instead of
// RESTProxyHandler's normal uniform forward.
func TestRESTProxyHandler_SDKGenerate_BlocksUnactivatedService(t *testing.T) {
	accountID := uuid.New()
	svcA := uuid.New()
	s := &sdkGenerateGateMockStore{accountID: accountID, activated: map[uuid.UUID]bool{}}
	fwd := &recordingForwarder{}
	handler := RESTProxyHandler(fwd, s)

	body := `{"selections":[{"service_id":"` + svcA.String() + `","service_name":"Stripe Billing","service_slug":"stripe-billing"}]}`
	req := httptest.NewRequest(http.MethodPost, "/sdks/generate", bytes.NewBufferString(body))
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if fwd.forwardCalled || fwd.forwardAndInspectCalled {
		t.Error("expected the request NOT to be forwarded to the Registry when a selection isn't activated")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "fused-cli workspace service add stripe-billing") {
		t.Errorf("expected friendly add command hint, got %s", rec.Body.String())
	}
}

func TestRESTProxyHandler_SDKGenerate_ForwardsWhenAllActivated(t *testing.T) {
	accountID := uuid.New()
	svcA := uuid.New()
	versionA := uuid.New()
	s := &sdkGenerateGateMockStore{
		accountID:   accountID,
		workspaceID: uuid.New(),
		activated:   map[uuid.UUID]bool{svcA: true},
		versions:    map[uuid.UUID][]store.WorkspaceServiceVersion{svcA: {{ServiceID: svcA, ServiceVersionID: versionA}}},
	}
	fwd := &recordingForwarder{}
	handler := RESTProxyHandler(fwd, s)

	body := `{"selections":[{"service_id":"` + svcA.String() + `","service_version_id":"` + versionA.String() + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/sdks/generate", bytes.NewBufferString(body))
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !fwd.forwardCalled {
		t.Error("expected the request to be forwarded to the Registry when every selection is activated")
	}
}
