package api

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

const unifiedWebhookRefresh = 5 * time.Second

// UnifiedAppWebhookWorker owns local subscriptions while JetStream owns durable delivery state across restarts.
type UnifiedAppWebhookWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// unifiedWebhookReceiver tracks the local pull loop independently of its persistent family consumer.
type unifiedWebhookReceiver struct {
	appID  uuid.UUID
	cancel context.CancelFunc
	done   chan struct{}
}

// StartUnifiedAppWebhookWorker attaches the existing WEBHOOKS stream to the shared hosted execution server.
func (s *EngineGRPCServer) StartUnifiedAppWebhookWorker(ctx context.Context) (*UnifiedAppWebhookWorker, error) {
	repository, ok := s.store.(store.UnifiedAppWebhookStore)
	// Automatic execution requires both authoritative scope and the existing durable event transport.
	if !ok || s.natsClient == nil || s.natsClient.JS == nil {
		return nil, errors.New("unified app webhook dependencies unavailable")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &UnifiedAppWebhookWorker{cancel: cancel, done: make(chan struct{})}
	go s.runUnifiedWebhookWorker(workerCtx, worker, repository)
	return worker, nil
}

// Stop cancels fetches and active runs before the shared execution runtime and NATS connection are closed.
func (worker *UnifiedAppWebhookWorker) Stop(ctx context.Context) {
	// Optional startup wiring can safely stop an absent worker.
	if worker == nil {
		return
	}
	worker.cancel()
	// Shutdown is bounded by the caller while the worker drains its execution goroutines.
	select {
	case <-worker.done:
	case <-ctx.Done():
	}
}

// runUnifiedWebhookWorker reconciles applied traffic and joins all local consumers on Engine shutdown.
func (s *EngineGRPCServer) runUnifiedWebhookWorker(ctx context.Context, worker *UnifiedAppWebhookWorker, repository store.UnifiedAppWebhookStore) {
	defer close(worker.done)
	receivers := make(map[uuid.UUID]unifiedWebhookReceiver)
	var running sync.WaitGroup
	// Every receiver shares cancellation and remains joined even after it leaves the desired snapshot.
	defer running.Wait()
	ticker := time.NewTicker(unifiedWebhookRefresh)
	defer ticker.Stop()
	// Durable subscriptions follow applied traffic independently of HTTP requests or connected clients.
	for {
		s.reconcileUnifiedWebhookReceivers(ctx, repository, receivers, &running)
		// Cancellation stops pending pulls and prevents a final fresh discovery pass.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcileUnifiedWebhookReceivers uses one authoritative snapshot and preserves existing consumers on storage failure.
func (s *EngineGRPCServer) reconcileUnifiedWebhookReceivers(ctx context.Context, repository store.UnifiedAppWebhookStore, receivers map[uuid.UUID]unifiedWebhookReceiver, running *sync.WaitGroup) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	targets, err := repository.ListUnifiedAppWebhookTargets(readCtx)
	// Logging only a fixed diagnostic avoids leaking config, payload, or transport error contents.
	if err != nil {
		slog.WarnContext(ctx, "Unified App webhook discovery failed")
		return
	}
	desired := make(map[uuid.UUID]struct{}, len(targets))
	// All desired targets came from one joined snapshot, so discovery avoids per-app config reads.
	for _, target := range targets {
		desired[target.FamilyID] = struct{}{}
		s.reconcileUnifiedWebhookReceiver(ctx, repository, target, receivers, running)
	}
	// Retire local pullers only after a complete authoritative target scan succeeds.
	for familyID, receiver := range receivers {
		// Removal stops local delivery; the durable cursor remains available if the family later regains a target.
		if _, exists := desired[familyID]; !exists {
			receiver.cancel()
			delete(receivers, familyID)
		}
	}
	// Small payload-free fences can expire without extending the normal 24-hour private result retention.
	if _, err := repository.DeleteExpiredUnifiedAppWebhookDeliveries(readCtx, time.Now(), 500); err != nil {
		slog.WarnContext(ctx, "Unified App webhook delivery cleanup failed")
	}
}

// reconcileUnifiedWebhookReceiver updates filters on promotion and restarts failed local fetch loops without resetting cursors.
func (s *EngineGRPCServer) reconcileUnifiedWebhookReceiver(ctx context.Context, repository store.UnifiedAppWebhookStore, target store.UnifiedAppWebhookTarget, receivers map[uuid.UUID]unifiedWebhookReceiver, running *sync.WaitGroup) {
	previous, exists := receivers[target.FamilyID]
	// Recheck broker filters even for a healthy puller: a racing peer must not leave stale version filters behind.
	if exists && previous.appID == target.AppID && unifiedWebhookReceiverRunning(previous) {
		// Reconciliation repairs shared broker drift without resetting this replica's durable cursor.
		if _, err := s.ensureUnifiedWebhookConsumer(target); err != nil {
			slog.WarnContext(ctx, "Unified App webhook filter reconciliation failed", "app_family_id", target.FamilyID)
		}
		return
	}
	// Replacing local fetchers leaves the family durable intact and lets admitted runs finish under their pinned identity.
	if exists {
		previous.cancel()
		delete(receivers, target.FamilyID)
	}
	subscription, err := s.subscribeUnifiedWebhook(target)
	// A failed broker update retries at the next bounded reconciliation pass.
	if err != nil {
		slog.WarnContext(ctx, "Unified App webhook subscription failed", "app_family_id", target.FamilyID)
		return
	}
	receiverCtx, cancel := context.WithCancel(ctx)
	receiver := unifiedWebhookReceiver{appID: target.AppID, cancel: cancel, done: make(chan struct{})}
	receivers[target.FamilyID] = receiver
	running.Add(1)
	// The goroutine owns its subscription and reports completion so reconciliation can recover broken connections.
	go func() {
		defer running.Done()
		defer close(receiver.done)
		defer subscription.Unsubscribe()
		s.pullUnifiedWebhooks(ctx, receiverCtx, repository, target.FamilyID, subscription)
	}()
}

// unifiedWebhookReceiverRunning distinguishes an idle healthy puller from a closed subscription needing recovery.
func unifiedWebhookReceiverRunning(receiver unifiedWebhookReceiver) bool {
	select {
	case <-receiver.done:
		return false
	default:
		return true
	}
}

// unifiedWebhookConsumerName shares one cursor per family across replicas and immutable versions.
func unifiedWebhookConsumerName(familyID uuid.UUID) string {
	return "unified-app-" + familyID.String()
}

// subscribeUnifiedWebhook binds a pull consumer to existing subjects without introducing ingress or another event stream.
func (s *EngineGRPCServer) subscribeUnifiedWebhook(target store.UnifiedAppWebhookTarget) (*nats.Subscription, error) {
	name, err := s.ensureUnifiedWebhookConsumer(target)
	// Binding may only follow successful exact-scope reconciliation.
	if err != nil {
		return nil, err
	}
	return s.natsClient.JS.PullSubscribe("", name, nats.Bind("WEBHOOKS", name), nats.ManualAck())
}

// ensureUnifiedWebhookConsumer repairs shared filters without replacing the family cursor or interrupting healthy pullers.
func (s *EngineGRPCServer) ensureUnifiedWebhookConsumer(target store.UnifiedAppWebhookTarget) (string, error) {
	subjects, err := unifiedWebhookSubjects(target)
	// An empty event set must never become NATS' implicit all-subjects filter.
	if err != nil {
		return "", err
	}
	// NATS treats absent filters as every subject, so an empty authored selection must fail closed.
	if len(subjects) == 0 {
		return "", errors.New("unified app has no selected webhook subjects")
	}
	name := unifiedWebhookConsumerName(target.FamilyID)
	js := s.natsClient.JS
	info, err := js.ConsumerInfo("WEBHOOKS", name)
	// First deployment starts at publication time, including events arriving before discovery finishes.
	if errors.Is(err, nats.ErrConsumerNotFound) {
		_, err = js.AddConsumer("WEBHOOKS", &nats.ConsumerConfig{
			Durable: name, FilterSubjects: subjects, AckPolicy: nats.AckExplicitPolicy,
			AckWait: 2 * time.Minute, MaxAckPending: 32, MaxDeliver: webhookBrokerMaxDeliver,
			// Retired families release broker state after retained stream messages and delivery fences expire.
			InactiveThreshold: 32 * 24 * time.Hour,
			DeliverPolicy:     nats.DeliverByStartTimePolicy, OptStartTime: &target.CreatedAt,
		})
	} else if err == nil && !reflect.DeepEqual(info.Config.FilterSubjects, subjects) {
		// Preserve start position and acknowledgement state while replacing the promoted version's filters.
		config := info.Config
		config.FilterSubjects = subjects
		_, err = js.UpdateConsumer("WEBHOOKS", &config)
	}
	// Unknown broker state cannot silently create an unfiltered consumer.
	if err != nil {
		return "", err
	}
	return name, nil
}

// pullUnifiedWebhooks leaves execution capacity to the shared app scheduler and bounds each receiver's in-flight delivery.
func (s *EngineGRPCServer) pullUnifiedWebhooks(engineCtx, receiverCtx context.Context, repository store.UnifiedAppWebhookStore, familyID uuid.UUID, subscription *nats.Subscription) {
	// Each family holds at most one local delivery while the shared scheduler owns execution concurrency.
	for receiverCtx.Err() == nil {
		fetchCtx, cancel := context.WithTimeout(receiverCtx, time.Second)
		messages, err := subscription.Fetch(1, nats.Context(fetchCtx))
		cancel()
		// Idle streams keep their durable cursor and do not produce warning noise.
		if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		// Reconciliation recreates a broken subscription; shutdown cancellation remains quiet.
		if err != nil {
			return
		}
		for _, message := range messages {
			// Promotion stops new pulls, but an admitted invocation completes with its own pinned version.
			s.handleUnifiedWebhook(engineCtx, repository, familyID, message)
		}
	}
}
