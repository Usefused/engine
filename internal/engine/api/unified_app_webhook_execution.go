package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// unifiedWebhookSubjects reuses provider subject construction and never implicitly subscribes to auth lifecycle events.
func unifiedWebhookSubjects(target store.UnifiedAppWebhookTarget) ([]string, error) {
	selections, err := models.DecodeAppSelections(target.ScopeSchemaVersion, target.Selections)
	// Invalid persisted scope cannot produce a wildcard subscription.
	if err != nil {
		return nil, err
	}
	var events []string
	// Each immutable service contributes only its explicitly selected provider-event scope.
	for _, selection := range selections {
		// Explicit all-events selection stays confined to one service and one registration label.
		if selection.WebhookSelectAll {
			events = append(events, selection.ServiceID.String()+".>")
			continue
		}
		// Finite selections preserve exact event names instead of widening to a service wildcard.
		for _, name := range selection.WebhookNames {
			// Reserved lifecycle subjects use a separate SDK-only authorization contract.
			if strings.HasPrefix(name, "fused.auth.") {
				continue
			}
			events = append(events, selection.ServiceID.String()+"."+name)
		}
	}
	subjects := buildFilterSubjects(target.AccountID, target.Attachment, events)
	sort.Strings(subjects)
	return subjects, nil
}

// unifiedWebhookMatches repeats exact attachment and immutable event authorization after every traffic change.
func unifiedWebhookMatches(target store.UnifiedAppWebhookTarget, message *nats.Msg) bool {
	parsed, valid := messaging.ParseWebhookSubject(message.Subject)
	// Provider triggers cannot consume Fused auth events or subjects from another workspace or registration.
	if !valid || parsed.FusedAuth {
		return false
	}
	prefix := "webhooks." + target.AccountID.String() + "." + parsed.ServiceID.String() + "." + subjectSafeLabel(target.Attachment) + "."
	// Comparing the full prefix prevents attachment names from broadening tenant scope.
	if !strings.HasPrefix(message.Subject, prefix) {
		return false
	}
	selections, err := models.DecodeAppSelections(target.ScopeSchemaVersion, target.Selections)
	// Malformed immutable state never authorizes a delivery.
	if err != nil {
		return false
	}
	return selectedWebhookEvent(selectedWebhookEvents(selections), parsed.ServiceID, parsed.EventName)
}

// reserveWebhookCapabilityRun reuses durable result admission without issuing a caller read capability to NATS.
func reserveWebhookCapabilityRun(ctx context.Context, results store.ExecutionResultStore, record store.ExecutionResult) (uuid.UUID, string, bool, error) {
	repository, ok := results.(store.UnifiedAppWebhookStore)
	// A store without atomic event ownership cannot safely start automatic effects.
	if !ok {
		return uuid.Nil, "", false, errors.New("unified app webhook store unavailable")
	}
	id, created, err := repository.ReserveUnifiedAppWebhookExecution(ctx, record)
	return id, "", created, err
}

// executeUnifiedWebhook pins new deliveries to current traffic and retains the existing run on broker redelivery.
func (s *EngineGRPCServer) executeUnifiedWebhook(ctx context.Context, repository store.UnifiedAppWebhookStore, familyID uuid.UUID, message *nats.Msg) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.webhook")
	defer span.End()
	span.SetAttributes(attribute.String("app.family_id", familyID.String()), attribute.String("execution.trigger", "webhook"))
	target, err := repository.GetUnifiedAppWebhookTarget(ctx, familyID)
	// Deactivated apps and removed attachments stop accepting new work immediately, before reconciliation catches up.
	if errors.Is(err, store.ErrAppRuntimeNotFound) {
		return nil
	}
	// Database uncertainty retains the broker delivery for retry.
	if err != nil {
		return err
	}
	// Changed selections may leave old filtered messages pending; they must never invoke the new version.
	if !unifiedWebhookMatches(*target, message) {
		return nil
	}
	spec, err := s.unifiedWebhookRunSpec(ctx, *target, message)
	// Missing bundles or invalid inputs cannot reserve provider-capable work.
	if err != nil {
		return err
	}
	admitted, requestErr := s.admitCapabilityRun(ctx, spec)
	// A failed atomic reservation leaves the event owned by JetStream.
	if requestErr != nil {
		span.SetStatus(codes.Error, "admission_failed")
		return errors.New(requestErr.code)
	}
	// An existing reservation already owns execution, even if its result body has expired or a version was promoted.
	if !admitted.created {
		span.SetAttributes(attribute.Bool("execution.redelivered", true))
		return nil
	}
	result, requestErr := s.runAdmittedCapability(ctx, spec, admitted)
	finishUnifiedAppSpan(span, result, requestErr)
	// A persistence failure can retry delivery; the durable reservation prevents repeating provider effects.
	if requestErr != nil {
		return errors.New(requestErr.code)
	}
	return nil
}

// unifiedWebhookRunSpec preserves the existing envelope and lets authored input validation own payload mapping.
func (s *EngineGRPCServer) unifiedWebhookRunSpec(ctx context.Context, target store.UnifiedAppWebhookTarget, message *nats.Msg) (capabilityRunSpec, error) {
	eventID := message.Header.Get("X-Webhook-Msg-ID")
	// Engine publishers supply a stable event ID; fabricating one would break redelivery ownership.
	if eventID == "" || len(eventID) > 256 || len(message.Data) > store.MaxExecutionDataBytes || !json.Valid(message.Data) {
		return capabilityRunSpec{}, store.ErrExecutionResultInvalid
	}
	bundle, manifest, found, requestErr := s.findUnifiedAppBundle(ctx, target.AppID)
	// A target must have the same source-verified bundle used by ordinary REST execution.
	if requestErr != nil {
		return capabilityRunSpec{}, errors.New(requestErr.code)
	}
	if !found {
		return capabilityRunSpec{}, store.ErrUnifiedAppBundleNotFound
	}
	return capabilityRunSpec{
		identity: auth.RuntimeIdentity{AccountID: target.AccountID, AppFamilyID: target.FamilyID, AppID: target.AppID,
			AppVersion: target.Version, Kind: store.AppKindUnifiedApp},
		version: target.Version, bundle: bundle, manifest: manifest, input: message.Data,
		mode: "live", transport: "webhook", webhookEventID: eventID,
	}, nil
}

// handleUnifiedWebhook uses existing broker retry limits while terminal authored failures remain inspectable results.
func (s *EngineGRPCServer) handleUnifiedWebhook(parent context.Context, repository store.UnifiedAppWebhookStore, familyID uuid.UUID, message *nats.Msg) {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	metadata, err := message.Metadata()
	// Invalid broker metadata cannot authorize an automatic invocation.
	if err != nil {
		_ = message.Term()
		return
	}
	// The existing final interception records exhausted infrastructure delivery without another app invocation.
	if webhookDeliveryExhausted(metadata.NumDelivered) {
		_ = message.Term()
		recordUnifiedWebhookExhaustion(ctx, message)
		return
	}
	err = s.executeUnifiedWebhook(ctx, repository, familyID, message)
	// Malformed envelopes are permanent failures; retrying identical bytes cannot fix them.
	if errors.Is(err, store.ErrExecutionResultInvalid) || errors.Is(err, store.ErrExecutionResultDataTooLarge) {
		_ = message.Term()
		return
	}
	// Admission and storage failures retain the existing five-second broker retry behavior.
	if err != nil {
		_ = message.NakWithDelay(5 * time.Second)
		return
	}
	// Durable execution ownership precedes acknowledgement, including a failed or indeterminate terminal result.
	_ = message.AckSync()
}

// recordUnifiedWebhookExhaustion feeds delivery failures into the existing provider webhook accounting path.
func recordUnifiedWebhookExhaustion(ctx context.Context, message *nats.Msg) {
	parsed, valid := messaging.ParseWebhookSubject(message.Subject)
	parts := strings.Split(message.Subject, ".")
	// Only validated provider subjects have the workspace and service identity required by canonical analytics.
	if !valid || parsed.FusedAuth {
		return
	}
	accountID, err := uuid.Parse(parts[1])
	// Invalid tenants cannot become durable execution receipts.
	if err != nil {
		return
	}
	publishFailedAnalytics(ctx, accountID, parsed.ServiceID, parsed.EventName, message.Header.Get("X-Webhook-Msg-ID"), message)
}
