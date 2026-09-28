package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/sandbox"
)

// replayTestDeterminism fixes source controls so fixture evidence can be compared exactly.
func replayTestDeterminism() sandbox.CapabilityDeterminism {
	return sandbox.CapabilityDeterminism{StartedAtUnixMs: 1700000000000, RandomSeed: 42}
}

// newReplayTestRecorder binds controls before any provider effect enters the fixture transcript.
func newReplayTestRecorder(t *testing.T, base sandbox.CapabilityScriptHost) *recordingCapabilityHost {
	t.Helper()
	recorder := NewRecordingCapabilityHost(base)
	if err := recorder.SetDeterminism(replayTestDeterminism()); err != nil {
		t.Fatalf("set replay controls: %v", err)
	}
	return recorder
}

type replayHostFixture struct {
	fetches  int
	dbReads  int
	dbWrites int
	data     json.RawMessage
	fail     bool
}

// Fetch exposes a deterministic provider result while counting live effects.
func (fixture *replayHostFixture) Fetch(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	fixture.fetches++
	// A provider failure may include sensitive text that recording must sanitize.
	if fixture.fail {
		return nil, errors.New("provider secret token sk_private")
	}
	return json.RawMessage(`{"customerId":"cus_123"}`), nil
}

// DBGet reads the fixture's live document for recorder tests.
func (fixture *replayHostFixture) DBGet(context.Context) (json.RawMessage, error) {
	fixture.dbReads++
	return fixture.data, nil
}

// DBSet writes the fixture's live document for recorder tests.
func (fixture *replayHostFixture) DBSet(_ context.Context, data json.RawMessage) error {
	fixture.dbWrites++
	fixture.data = data
	return nil
}

// TestCapabilityReplayMatchesWithoutEffects verifies exact calls and temporary DB state.
func TestCapabilityReplayMatchesWithoutEffects(t *testing.T) {
	ctx := context.Background()
	base := &replayHostFixture{data: json.RawMessage("null")}
	recorder := newReplayTestRecorder(t, base)
	request := json.RawMessage(`{"service":"crm","operation":"upsert","input":{"id":"cus_123"}}`)
	response, history := recordReplayFixture(t, ctx, recorder, request)
	assertReplayMatchesWithoutEffects(t, ctx, base, history, response)
}

// recordReplayFixture captures one live call and document write for replay assertions.
func recordReplayFixture(t *testing.T, ctx context.Context, recorder *recordingCapabilityHost, request json.RawMessage) (json.RawMessage, json.RawMessage) {
	t.Helper()
	response, err := recorder.Fetch(ctx, request)
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	if err := recorder.DBSet(ctx, json.RawMessage(`{"customerId":"cus_123"}`)); err != nil {
		t.Fatalf("set live data: %v", err)
	}
	history, err := recorder.History()
	if err != nil {
		t.Fatalf("record history: %v", err)
	}
	return response, history
}

// assertReplayMatchesWithoutEffects checks exact provider response and temporary DB isolation.
func assertReplayMatchesWithoutEffects(t *testing.T, ctx context.Context, base *replayHostFixture, history, response json.RawMessage) {
	t.Helper()
	replay, err := NewReplayCapabilityHost(history)
	if err != nil {
		t.Fatalf("load history %s: %v", history, err)
	}
	// Replay must recover the exact worker controls from encrypted evidence.
	if replay.Determinism() != replayTestDeterminism() {
		t.Fatalf("replay controls changed: %+v", replay.Determinism())
	}
	replayed, err := replay.Fetch(ctx, json.RawMessage(`{"input":{"id":"cus_123"},"operation":"upsert","service":"crm"}`))
	if err != nil || !bytes.Equal(replayed, response) {
		t.Fatalf("expected canonical replay response, got %q error=%v", replayed, err)
	}
	if err := replay.DBSet(ctx, json.RawMessage(`{"customerId":"temporary"}`)); err != nil {
		t.Fatalf("set replay data: %v", err)
	}
	assertReplayTemporaryData(t, ctx, replay, base)
	if err := replay.Consumed(); err != nil {
		t.Fatalf("expected consumed history: %v", err)
	}
}

// TestCapabilityReplayRequiresControls prevents evidence from using ambient worker time or randomness.
func TestCapabilityReplayRequiresControls(t *testing.T) {
	recorder := NewRecordingCapabilityHost(&replayHostFixture{})
	if _, err := recorder.History(); !errors.Is(err, ErrCapabilityReplayInvalid) {
		t.Fatalf("missing recording controls error = %v", err)
	}
	if _, err := NewReplayCapabilityHost(json.RawMessage(`{"version":2,"calls":[]}`)); !errors.Is(err, ErrCapabilityReplayInvalid) {
		t.Fatalf("missing replay controls error = %v", err)
	}
}

// assertReplayTemporaryData proves the replay overlay has no live DB or provider effects.
func assertReplayTemporaryData(t *testing.T, ctx context.Context, replay *replayCapabilityHost, base *replayHostFixture) {
	t.Helper()
	data, err := replay.DBGet(ctx)
	if err != nil || string(data) != `{"customerId":"temporary"}` {
		t.Fatalf("expected temporary replay data, got %q error=%v", data, err)
	}
	if base.fetches != 1 || base.dbWrites != 1 || string(base.data) != `{"customerId":"cus_123"}` {
		t.Fatalf("replay touched live host: %#v", base)
	}
}

// TestCapabilityReplayRejectsDivergence rejects changed or omitted provider calls.
func TestCapabilityReplayRejectsDivergence(t *testing.T) {
	ctx := context.Background()
	recorder := newReplayTestRecorder(t, &replayHostFixture{})
	_, _ = recorder.Fetch(ctx, json.RawMessage(`{"operation":"create"}`))
	history, err := recorder.History()
	if err != nil {
		t.Fatalf("record history: %v", err)
	}
	replay, err := NewReplayCapabilityHost(history)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if _, err := replay.Fetch(ctx, json.RawMessage(`{"operation":"delete"}`)); !errors.Is(err, ErrCapabilityReplayDiverged) {
		t.Fatalf("expected changed-call divergence, got %v", err)
	}
	if err := replay.Consumed(); !errors.Is(err, ErrCapabilityReplayDiverged) {
		t.Fatalf("expected incomplete replay divergence, got %v", err)
	}
}

// TestCapabilityReplayScrubsProviderErrors records a generic failure without provider credentials.
func TestCapabilityReplayScrubsProviderErrors(t *testing.T) {
	ctx := context.Background()
	recorder := newReplayTestRecorder(t, &replayHostFixture{fail: true})
	_, err := recorder.Fetch(ctx, json.RawMessage(`{"operation":"create"}`))
	if !errors.Is(err, capabilityRecordedFetchError) {
		t.Fatalf("expected generic worker error, got %v", err)
	}
	history, err := recorder.History()
	if err != nil || strings.Contains(string(history), "sk_private") {
		t.Fatalf("history contains provider text or failed: %q, %v", history, err)
	}
	replay, err := NewReplayCapabilityHost(history)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if _, err := replay.Fetch(ctx, json.RawMessage(`{"operation":"create"}`)); !errors.Is(err, capabilityRecordedFetchError) {
		t.Fatalf("expected same generic replay error, got %v", err)
	}
}

// TestCapabilityReplayPreservesParallelCompletionOrder verifies results follow recorded completion, not call order.
func TestCapabilityReplayPreservesParallelCompletionOrder(t *testing.T) {
	history := json.RawMessage(`{"version":2,"determinism":{"startedAtUnixMs":"1700000000000","randomSeed":"42"},"calls":[{"ordinal":1,"request":{"call":1},"response":"first","completion":2},{"ordinal":2,"request":{"call":2},"response":"second","completion":1}]}`)
	replay, err := NewReplayCapabilityHost(history)
	if err != nil {
		t.Fatalf("load parallel history: %v", err)
	}
	firstDone := make(chan json.RawMessage, 1)
	go func() {
		response, _ := replay.Fetch(context.Background(), json.RawMessage(`{"call":1}`))
		firstDone <- response
	}()
	waitReplayCalls(t, replay, 1)
	select {
	case <-firstDone:
		t.Fatal("first result completed before the recorded second result")
	default:
	}
	second, err := replay.Fetch(context.Background(), json.RawMessage(`{"call":2}`))
	if err != nil || string(second) != `"second"` {
		t.Fatalf("expected recorded second completion, got %q error=%v", second, err)
	}
	select {
	case first := <-firstDone:
		if string(first) != `"first"` {
			t.Fatalf("expected first result after second, got %q", first)
		}
	case <-time.After(time.Second):
		t.Fatal("first result did not complete after second")
	}
	if err := replay.Consumed(); err != nil {
		t.Fatalf("parallel evidence was not consumed: %v", err)
	}
}

// waitReplayCalls observes call admission without relying on scheduler timing.
func waitReplayCalls(t *testing.T, replay *replayCapabilityHost, count int) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		replay.mu.Lock()
		seen := replay.nextCall
		replay.mu.Unlock()
		if seen >= count {
			return
		}
		select {
		case <-deadline:
			t.Fatal("replay call did not start")
		case <-ticker.C:
		}
	}
}
