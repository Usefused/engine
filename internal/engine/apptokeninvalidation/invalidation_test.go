package apptokeninvalidation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type publishStub struct {
	message *nats.Msg
	err     error
	order   *[]string
}

func (s *publishStub) PublishMsgJS(message *nats.Msg) (*nats.PubAck, error) {
	s.message = message
	if s.order != nil {
		*s.order = append(*s.order, "publish")
	}
	return &nats.PubAck{}, s.err
}

type revocationRepositoryStub struct {
	revocation store.AppTokenRevocation
	err        error
	order      *[]string
}

func (s *revocationRepositoryStub) RevokeAppToken(_ context.Context, _ uuid.UUID, _ string) (*store.AppTokenRevocation, error) {
	if s.order != nil {
		*s.order = append(*s.order, "delete")
	}
	if s.err != nil {
		return nil, s.err
	}
	return &s.revocation, nil
}

type invalidatorStub struct {
	mu       sync.Mutex
	tokenIDs []uuid.UUID
	order    *[]string
}

func (s *invalidatorStub) InvalidateToken(tokenID uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenIDs = append(s.tokenIDs, tokenID)
	if s.order != nil {
		*s.order = append(*s.order, "invalidate")
	}
	return 1
}

func (s *invalidatorStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokenIDs)
}

func TestServiceRevokesThenEvictsLocallyThenPublishesPreciseEvent(t *testing.T) {
	order := []string{}
	revocation := store.AppTokenRevocation{TokenID: uuid.New(), AppFamilyID: uuid.New(), RevokedAt: time.Now().UTC()}
	repository := &revocationRepositoryStub{revocation: revocation, order: &order}
	invalidator := &invalidatorStub{order: &order}
	transport := &publishStub{order: &order}
	service := mustNewService(t, repository, invalidator, NewPublisher(transport))

	got, err := service.RevokeAppToken(context.Background(), revocation.AppFamilyID, "private-user-label")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got.TokenID != revocation.TokenID || strings.Join(order, ",") != "delete,invalidate,publish" {
		t.Fatalf("revocation/order = %#v/%v", got, order)
	}
	assertPublishedEventContract(t, transport.message)
}

func assertPublishedEventContract(t *testing.T, message *nats.Msg) {
	t.Helper()
	if message.Subject != messaging.AppTokenInvalidatedSubject {
		t.Fatalf("subject = %q", message.Subject)
	}
	if message.Header.Get(nats.MsgIdHdr) == "" {
		t.Fatal("JetStream de-duplication ID is missing")
	}
	var fields map[string]any
	if err := json.Unmarshal(message.Data, &fields); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	for _, key := range []string{"event_id", "token_id", "app_family_id", "revoked_at"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("event missing %q: %s", key, message.Data)
		}
	}
	if len(fields) != 4 || strings.Contains(string(message.Data), "private-user-label") {
		t.Fatalf("event contains fields outside the bounded contract: %s", message.Data)
	}
}

func TestServiceKeepsCommittedRevocationWhenPublishFails(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	revocation := store.AppTokenRevocation{TokenID: uuid.New(), AppFamilyID: uuid.New(), RevokedAt: time.Now().UTC()}
	invalidator := &invalidatorStub{}
	service := mustNewService(t,
		&revocationRepositoryStub{revocation: revocation}, invalidator,
		NewPublisher(&publishStub{err: errors.New("nats credentials private detail")}),
	)

	if _, err := service.RevokeAppToken(context.Background(), revocation.AppFamilyID, "private-user-label"); err != nil {
		t.Fatalf("publish failure undid committed revocation: %v", err)
	}
	if invalidator.count() != 1 {
		t.Fatalf("local invalidations = %d, want 1", invalidator.count())
	}
	for _, span := range recorder.Ended() {
		for _, attr := range span.Attributes() {
			value := attr.Value.Emit()
			if strings.Contains(value, "private-user-label") || strings.Contains(value, "credentials private detail") || strings.Contains(value, revocation.TokenID.String()) {
				t.Fatalf("span %q exposed prohibited revocation material in %q", span.Name(), attr.Key)
			}
		}
	}
}

func TestServiceStopsAfterPersistenceFailure(t *testing.T) {
	order := []string{}
	service := mustNewService(t,
		&revocationRepositoryStub{err: errors.New("database unavailable"), order: &order},
		&invalidatorStub{order: &order},
		NewPublisher(&publishStub{order: &order}),
	)
	if _, err := service.RevokeAppToken(context.Background(), uuid.New(), "private-user-label"); err == nil {
		t.Fatal("persistence failure was ignored")
	}
	if got := strings.Join(order, ","); got != "delete" {
		t.Fatalf("side-effect order = %q, want only delete", got)
	}
}

func TestConsumeInvalidatesOnlyEventToken(t *testing.T) {
	invalidator := &invalidatorStub{}
	event := Event{EventID: uuid.New(), TokenID: uuid.New(), AppFamilyID: uuid.New(), RevokedAt: time.Now().UTC()}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	consume(&nats.Msg{Data: payload}, invalidator)
	consume(&nats.Msg{Data: []byte(`{"token_id":"not-a-uuid"}`)}, invalidator)
	if invalidator.count() != 1 || invalidator.tokenIDs[0] != event.TokenID {
		t.Fatalf("invalidated token IDs = %v, want only %s", invalidator.tokenIDs, event.TokenID)
	}
}

func TestConsumeRejectsContractExtensionsAndTrailingDocuments(t *testing.T) {
	invalidator := &invalidatorStub{}
	event := Event{EventID: uuid.New(), TokenID: uuid.New(), AppFamilyID: uuid.New(), RevokedAt: time.Now().UTC()}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{`,"token_hash":"secret"}`, ` {}`} {
		candidate := payload
		if suffix[0] == ',' {
			candidate = append(append([]byte(nil), payload[:len(payload)-1]...), suffix...)
		} else {
			candidate = append(append([]byte(nil), payload...), suffix...)
		}
		consume(&nats.Msg{Data: candidate}, invalidator)
	}
	if invalidator.count() != 0 {
		t.Fatalf("invalid contract documents caused %d invalidations", invalidator.count())
	}
}

func TestJetStreamFanoutInvalidatesEveryLiveReplica(t *testing.T) {
	connection, js := newTestJetStream(t)
	first, second := &invalidatorStub{}, &invalidatorStub{}
	startTestWorker(t, js, first)
	startTestWorker(t, js, second)

	revocation := store.AppTokenRevocation{TokenID: uuid.New(), AppFamilyID: uuid.New(), RevokedAt: time.Now().UTC()}
	if err := NewPublisher(&messaging.NATSClient{Conn: connection, JS: js}).Publish(context.Background(), revocation); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitForFanout(t, first, second)
}

func newTestJetStream(t *testing.T) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	if !natsServer.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)

	connection, err := nats.Connect(natsServer.ClientURL())
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(connection.Close)
	js, err := connection.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{
		Name: messaging.FusedEngineStream, Subjects: messaging.FusedEngineStreamSubjects(), Storage: nats.MemoryStorage,
	}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	return connection, js
}

func startTestWorker(t *testing.T, js nats.JetStreamContext, invalidator Invalidator) {
	t.Helper()
	worker, err := StartWorker(js, invalidator)
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(worker.Stop)
}

func waitForFanout(t *testing.T, first, second *invalidatorStub) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (first.count() != 1 || second.count() != 1) {
		time.Sleep(5 * time.Millisecond)
	}
	if first.count() != 1 || second.count() != 1 {
		t.Fatalf("fanout counts = %d/%d, want 1/1", first.count(), second.count())
	}
}

func mustNewService(t *testing.T, repository repository, invalidator Invalidator, publisher *Publisher) *Service {
	t.Helper()
	service, err := NewService(repository, invalidator, publisher)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}
