package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TestUnifiedAppThrownMessageProjection exercises actual JavaScript errors through the public result projection.
func TestUnifiedAppThrownMessageProjection(t *testing.T) {
	bundle := `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>{throw new Error("Customer account is suspended")}};`
	_, runErr := executionappvm.RunInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), &diagnosticProviderHost{})
	status, code := capabilityCompletion(runErr, nil, false)
	record := &store.ExecutionResult{Status: status, ErrorCode: code, ErrorMessage: capabilityPublicError(code, runErr)}
	body, _ := json.Marshal(projectCapabilityExecution(record, ""))
	// The caller gets the authored explanation, not stack frames or the private error object.
	if !strings.Contains(string(body), "Customer account is suspended") || strings.Contains(string(body), "stack") || status != "failed" {
		t.Fatalf("unexpected result: %s", body)
	}
	// Infrastructure errors must not accidentally become authored exception text.
	if strings.Contains(capabilityPublicError("execution_failed", errors.New("private database URI")), "private") {
		t.Fatal("infrastructure error exposed")
	}
}

type credentialFailureHost struct{ replayHostFixture }

// Fetch models a missing credential without a provider request or secret material.
func (*credentialFailureHost) Fetch(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, &sandbox.CredentialMaterialMissingError{ServiceID: uuid.New(), BucketID: uuid.New(), ServiceSlug: "stripe", BucketName: "production", AuthType: "basic", AuthName: "basicAuth"}
}

// TestCredentialFailureReplaysReadableCommand verifies recovery survives authored execution and deterministic replay.
func TestCredentialFailureReplaysReadableCommand(t *testing.T) {
	host := newReplayTestRecorder(t, &credentialFailureHost{})
	request := json.RawMessage(`{"operation":"create"}`)
	_, failure := host.Fetch(context.Background(), request)
	history, err := host.History()
	// The transcript must admit the new recovery error contract before a replay can be constructed.
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewReplayCapabilityHost(history)
	if err != nil {
		t.Fatal(err)
	}
	_, replayErr := replay.Fetch(context.Background(), request)
	// Live and replayed code must see the same message, including readable targets.
	if failure == nil || replayErr == nil || failure.Error() != replayErr.Error() || !strings.Contains(failure.Error(), "secret set 'stripe' --bucket 'production'") {
		t.Fatalf("live %v replay %v", failure, replayErr)
	}
}

type failureRouteStore struct {
	diagnosticRouteStore
	resultReads int
}

// GetExecutionResult supplies a private payload to prove the message endpoint cannot project it.
func (s *failureRouteStore) GetExecutionResult(_ context.Context, accountID, appID, _ uuid.UUID) (*store.ExecutionResult, error) {
	s.resultReads++
	// The exact account and version must reach the store, not a caller-controlled family alias.
	if accountID != s.app.AccountID || appID != s.app.AppID {
		return nil, store.ErrExecutionResultNotFound
	}
	return &store.ExecutionResult{ErrorCode: "execution_failed", ErrorMessage: "Customer account is suspended", Input: json.RawMessage(`{"private":"request"}`), Output: json.RawMessage(`{"private":"response"}`)}, nil
}

// TestExecutionFailurePermissions keeps ordinary errors useful while denying cross-family and unaudited reads.
func TestExecutionFailurePermissions(t *testing.T) {
	account, family, appID := uuid.New(), uuid.New(), uuid.New()
	for _, test := range []struct {
		name                 string
		read, audit, foreign bool
		status               int
	}{
		{"receipt reader", true, true, false, 200}, {"no audit", true, false, false, 403}, {"no app read", false, true, false, 403}, {"other family", true, true, true, 403},
	} {
		// Every case uses a fresh authorization snapshot and read counters.
		t.Run(test.name, func(t *testing.T) {
			fixture := &failureRouteStore{diagnosticRouteStore: diagnosticRouteStore{app: store.App{AppID: appID, AppFamilyID: family, AccountID: account}}}
			grantFamily := family
			// A grant to another family must not authorize this exact version.
			if test.foreign {
				grantFamily = uuid.New()
			}
			grants := []accesscontrol.Grant{}
			// App visibility alone is deliberately insufficient to read execution history.
			if test.read {
				grants = append(grants, accesscontrol.Grant{Permission: accesscontrol.PermissionAppUnifiedAppRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: grantFamily}})
			}
			// Audit permission remains independent from ordinary app visibility.
			if test.audit {
				grants = append(grants, accesscontrol.Grant{Permission: accesscontrol.PermissionAuditRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: grantFamily}})
			}
			actor := controlTestOwnerActor(account)
			actor.Authorization, _ = appPermissionTestSnapshot(1, grants...)
			router := chi.NewRouter()
			router.Get("/apps/{app_id}/executions/{execution_id}/failure", ExecutionFailureHandler(fixture))
			request := httptest.NewRequest(http.MethodGet, "/apps/"+appID.String()+"/executions/"+uuid.NewString()+"/failure", nil)
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// The narrow response must never retrieve diagnostics or include request/response data.
			if response.Code != test.status || fixture.reads != 0 || strings.Contains(response.Body.String(), "private") {
				t.Fatalf("status %d body %s", response.Code, response.Body.String())
			}
			// Authorization must fail before retained results are loaded.
			if test.status != 200 && fixture.resultReads != 0 {
				t.Fatal("denied result was read")
			}
			// Successful reads include the message and disable shared caching.
			if test.status == 200 && (!strings.Contains(response.Body.String(), "Customer account is suspended") || response.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("missing error: %s", response.Body.String())
			}
		})
	}
}

// TestCredentialFailureReachesAuthoredException exercises the VM bridge rather than only the host return value.
func TestCredentialFailureReachesAuthoredException(t *testing.T) {
	host := newReplayTestRecorder(t, &credentialFailureHost{})
	bundle := `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>{return await __fusedHost.fetch(JSON.stringify({operation:"create"}))}};`
	_, runErr := executionappvm.RunInProcess(context.Background(), []byte(bundle), json.RawMessage(`{}`), host)
	message := capabilityPublicError("execution_failed", runErr)
	// A failed await must deliver actionable guidance instead of either generic masking layer.
	if !strings.Contains(message, "secret set 'stripe' --bucket 'production'") {
		t.Fatalf("masked credential failure: %s", message)
	}
}

// TestProviderFailureReachesCallerAndReplay verifies failed awaits and authored catches receive the real explanation.
func TestProviderFailureReachesCallerAndReplay(t *testing.T) {
	for _, execute := range []string{
		`return await __fusedHost.fetch('{"operation":"create"}')`,
		`try { await __fusedHost.fetch('{"operation":"create"}') } catch (error) { throw new Error("Checkout failed: " + error.message) }`,
	} {
		host := newReplayTestRecorder(t, &diagnosticProviderHost{})
		bundle := []byte(`globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>{` + execute + `}};`)
		_, runErr := executionappvm.RunInProcess(context.Background(), bundle, json.RawMessage(`{}`), host)
		message := capabilityPublicError("execution_failed", runErr)
		// Both the direct rejection and an authored wrapper must preserve the provider's explanation.
		if !strings.Contains(message, "provider error explanation") || strings.Contains(message, "private body") {
			t.Fatalf("unexpected live message: %q", message)
		}
		history, err := host.History()
		// Replay evidence must be complete before a no-effects host can be constructed.
		if err != nil {
			t.Fatal(err)
		}
		replay, err := NewReplayCapabilityHost(history)
		// Selected messages must pass replay validation without admitting private response bodies.
		if err != nil {
			t.Fatal(err)
		}
		_, replayErr := executionappvm.RunInProcess(context.Background(), bundle, json.RawMessage(`{}`), replay)
		// Error-sensitive application logic must behave identically during replay.
		if got := capabilityPublicError("execution_failed", replayErr); got != message {
			t.Fatalf("replay changed the message: %q != %q", got, message)
		}
	}
}

// TestReplayRejectsInvalidErrorMessages prevents new explanations from bypassing outcome and size invariants.
func TestReplayRejectsInvalidErrorMessages(t *testing.T) {
	for _, call := range []capabilityReplayCall{
		{Ordinal: 1, Completion: 1, Request: json.RawMessage(`{}`), Response: json.RawMessage(`{}`), ErrorMessage: "failure on a successful call"},
		{Ordinal: 1, Completion: 1, Request: json.RawMessage(`{}`), Error: capabilityRecordedFetchError.Error(), ErrorMessage: strings.Repeat("x", 65537)},
	} {
		// A successful result or oversized message cannot define an authored failure during replay.
		if validateReplayCall(call, 1) == nil {
			t.Fatal("invalid failure envelope accepted")
		}
	}
}
