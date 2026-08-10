package applifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
)

type recordingConfigPlanRepository struct {
	params store.ApplyAppConfigPlanParams
	result *store.ApplyAppConfigPlanResult
	err    error
}

type recordingTokenRepository struct {
	Repository
	tokenHash string
	tokenName string
	policy    store.AppTokenPolicy
	token     *store.AppTokenMetadata
}

type recordingLifecycleRepository struct {
	Repository
	family store.AppFamily
	app    store.App
}

func (repository *recordingLifecycleRepository) CreateOrGetAppFamily(_ context.Context, family store.AppFamily) (*store.AppFamily, bool, error) {
	repository.family = family
	return &repository.family, true, nil
}

func (repository *recordingLifecycleRepository) PublishAppVersion(_ context.Context, app store.App) (*store.App, bool, error) {
	repository.app = app
	return &repository.app, true, nil
}

func (repository *recordingTokenRepository) CreateAppToken(_ context.Context, familyID uuid.UUID, tokenHash, name string, policy store.AppTokenPolicy) (*store.AppTokenMetadata, error) {
	repository.tokenHash = tokenHash
	repository.tokenName = name
	repository.policy = policy
	repository.token = &store.AppTokenMetadata{
		ID: uuid.New(), AppFamilyID: familyID, Name: name,
		AppTokenPolicy: policy, CreatedAt: time.Now(),
	}
	return repository.token, nil
}

func (repository *recordingConfigPlanRepository) ApplyAppConfigPlan(_ context.Context, params store.ApplyAppConfigPlanParams) (*store.ApplyAppConfigPlanResult, error) {
	repository.params = params
	return repository.result, repository.err
}

func TestApplyConfigPlanRecordsSafeLifecycleTelemetry(t *testing.T) {
	recorder := recordLifecycleSpans(t)

	appID := uuid.New()
	repository := &recordingConfigPlanRepository{result: &store.ApplyAppConfigPlanResult{
		AppID: appID, VersionCreated: true, TokenCreated: true,
	}}
	params := store.ApplyAppConfigPlanParams{Scope: store.AppRuntime{
		AppID: appID, Kind: store.AppKindMCP, Version: "1.0.0",
	}}
	result, err := New(nil).ApplyConfigPlan(context.Background(), repository, params)
	if err != nil {
		t.Fatalf("ApplyConfigPlan: %v", err)
	}
	if result != repository.result || repository.params.Scope.AppID != appID {
		t.Fatal("atomic apply request or result was not preserved")
	}

	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "engine.applifecycle.apply_config_plan" {
		t.Fatalf("unexpected lifecycle spans: %#v", spans)
	}
	attributes := spanAttributes(spans[0].Attributes())
	if attributes["app.id"] != appID.String() || attributes["app.kind"] != "mcp" || attributes["app.version"] != "1.0.0" || attributes["outcome"] != "success" {
		t.Fatalf("unexpected safe lifecycle attributes: %#v", attributes)
	}
	for key := range attributes {
		for _, prohibited := range []string{"token", "hash", "selection", "source"} {
			if strings.Contains(key, prohibited) {
				t.Fatalf("prohibited lifecycle attribute %q was emitted", key)
			}
		}
	}
}

func TestGenerateTokenPersistsOnlyHashAndRecordsOutcome(t *testing.T) {
	recorder := recordLifecycleSpans(t)
	familyID := uuid.New()
	repository := &recordingTokenRepository{}
	plaintext, token, err := New(repository).GenerateToken(context.Background(), GenerateTokenParams{
		AppFamilyID: familyID,
		Name:        "automation",
	})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if token != repository.token || repository.tokenHash != auth.HashToken(plaintext) || repository.tokenHash == plaintext {
		t.Fatal("family token persistence did not preserve the one-time plaintext boundary")
	}
	if repository.tokenName != "automation" {
		t.Fatalf("token name = %q, want automation", repository.tokenName)
	}
	if !repository.policy.AllowAll || len(repository.policy.AllowedOperations) != 0 || repository.policy.ExpiresAt != nil {
		t.Fatalf("default token policy = %#v, want unrestricted non-expiring policy", repository.policy)
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "engine.applifecycle.generate_token" {
		t.Fatalf("unexpected token spans: %#v", spans)
	}
	attributes := spanAttributes(spans[0].Attributes())
	if attributes["app.family_id"] != familyID.String() || attributes["outcome"] != "created" {
		t.Fatalf("unexpected token attributes: %#v", attributes)
	}
	for key := range attributes {
		for _, prohibited := range []string{"hash", "plaintext", "allowed_operations"} {
			if strings.Contains(key, prohibited) {
				t.Fatalf("token secret or scope leaked into OTEL attribute %q", key)
			}
		}
	}
}

func TestResolveTokenPolicyNormalizesStrictAllowAndExpiry(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	duration := 15 * time.Minute
	policy, err := resolveTokenPolicy([]string{" users.read ", "users.write", "users.read"}, &duration, now)
	if err != nil {
		t.Fatalf("resolveTokenPolicy: %v", err)
	}
	if policy.AllowAll {
		t.Fatal("strict allowlist unexpectedly resolved to full access")
	}
	if got, want := strings.Join(policy.AllowedOperations, ","), "users.read,users.write"; got != want {
		t.Fatalf("allowed operations = %q, want %q", got, want)
	}
	if policy.ExpiresAt == nil || !policy.ExpiresAt.Equal(now.Add(duration)) {
		t.Fatalf("expiry = %v, want %v", policy.ExpiresAt, now.Add(duration))
	}
}

func TestResolveTokenPolicyRejectsAmbiguousAllow(t *testing.T) {
	for _, allow := range [][]string{{}, {""}, {"*", "users.read"}} {
		if _, err := resolveTokenPolicy(allow, nil, time.Now()); !errors.Is(err, ErrTokenPolicyInvalid) {
			t.Fatalf("resolveTokenPolicy(%q) error = %v, want ErrTokenPolicyInvalid", allow, err)
		}
	}
}

func TestLifecycleMutationsRecordSafeOutcomes(t *testing.T) {
	recorder := recordLifecycleSpans(t)
	repository := &recordingLifecycleRepository{}
	service := New(repository)
	family, created, err := service.CreateOrGetFamily(context.Background(), CreateFamilyParams{
		AccountID: uuid.New(), Kind: store.AppKindSDK, CanonicalName: "billing", TargetLanguage: "go", OwnerSubjectID: uuid.New(),
	})
	if err != nil || !created {
		t.Fatalf("CreateOrGetFamily() = (%v, %t, %v), want created family", family, created, err)
	}
	_, err = service.PublishVersion(context.Background(), PublishVersionParams{
		AppFamilyID: family.AppFamilyID, AccountID: family.AccountID, AppID: uuid.New(), Kind: store.AppKindSDK,
		Version: "1.0.0", SourceHash: "source", Selections: []byte(`[]`), ScopeSchemaVersion: 1,
	})
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("lifecycle span count = %d, want 2", len(spans))
	}
	createAttributes := spanAttributes(spans[0].Attributes())
	publishAttributes := spanAttributes(spans[1].Attributes())
	if createAttributes["outcome"] != "created" || createAttributes["app.kind"] != "sdk" {
		t.Fatalf("unexpected create attributes: %#v", createAttributes)
	}
	if publishAttributes["outcome"] != "created" || publishAttributes["app.kind"] != "sdk" {
		t.Fatalf("unexpected publish attributes: %#v", publishAttributes)
	}
}

func TestNewExecutionTokenUsesSharedFamilyTokenShape(t *testing.T) {
	first, firstHash, err := NewExecutionToken()
	if err != nil {
		t.Fatalf("NewExecutionToken: %v", err)
	}
	second, secondHash, err := NewExecutionToken()
	if err != nil {
		t.Fatalf("NewExecutionToken second call: %v", err)
	}
	if first == second || firstHash == secondHash {
		t.Fatal("execution tokens must be independently random")
	}
	if len(first) < len("fused-app-")+40 || first[:len("fused-app-")] != "fused-app-" {
		t.Fatalf("unexpected family token shape: %q", first)
	}
	if firstHash != auth.HashToken(first) {
		t.Fatal("returned token hash does not match the shared validator hash")
	}
}

func TestApplyConfigPlanRejectsInvalidKindBeforePersistence(t *testing.T) {
	repository := &recordingConfigPlanRepository{}
	_, err := New(nil).ApplyConfigPlan(context.Background(), repository, store.ApplyAppConfigPlanParams{
		Scope: store.AppRuntime{AppID: uuid.New(), Kind: store.AppKind("artifact")},
	})
	if err != store.ErrAppKindInvalid {
		t.Fatalf("ApplyConfigPlan error = %v, want ErrAppKindInvalid", err)
	}
	if repository.params.Scope.AppID != uuid.Nil {
		t.Fatal("invalid app kind reached persistence")
	}
}

func spanAttributes(values []attribute.KeyValue) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		if value.Value.Type() == attribute.STRING {
			result[string(value.Key)] = value.Value.AsString()
		}
	}
	return result
}

func recordLifecycleSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return recorder
}
