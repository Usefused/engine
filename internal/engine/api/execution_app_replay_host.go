package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
)

var (
	ErrCapabilityReplayDiverged  = errors.New("capability replay diverged from recorded calls")
	ErrCapabilityReplayInvalid   = errors.New("capability replay evidence is invalid")
	capabilityRecordedFetchError = errors.New("capability workspace operation failed")
)

type capabilityReplayCall struct {
	Ordinal    int             `json:"ordinal"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response,omitempty"`
	Error      string          `json:"error,omitempty"`
	Completion int             `json:"completion"`
}

type capabilityReplayHistory struct {
	Version     int                           `json:"version"`
	Determinism sandbox.CapabilityDeterminism `json:"determinism"`
	Calls       []capabilityReplayCall        `json:"calls"`
}

type recordingCapabilityHost struct {
	phases           []executionappvm.PhaseTiming
	base             sandbox.CapabilityScriptHost
	mu               sync.Mutex
	dbMu             sync.Mutex
	diagnosticErrors map[int]*executionappvm.DiagnosticError
	calls            []capabilityReplayCall
	completed        int
	data             json.RawMessage
	invalid          bool
	determinism      sandbox.CapabilityDeterminism
}

type replayCapabilityHost struct {
	mu          sync.Mutex
	calls       []capabilityReplayCall
	nextCall    int
	consumed    map[int]struct{}
	completion  []chan struct{}
	data        json.RawMessage
	diverged    bool
	determinism sandbox.CapabilityDeterminism
}

// NewRecordingCapabilityHost wraps the live host without changing its DB or provider ownership.
func NewRecordingCapabilityHost(base sandbox.CapabilityScriptHost) *recordingCapabilityHost {
	return &recordingCapabilityHost{base: base, data: json.RawMessage("null")}
}

// SetDeterminism binds the same Engine-created controls used by the worker to encrypted replay history.
func (host *recordingCapabilityHost) SetDeterminism(control sandbox.CapabilityDeterminism) error {
	// Invalid or absent controls must never produce evidence that replays with ambient time or randomness.
	if host == nil || !control.Valid() {
		return ErrCapabilityReplayInvalid
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	// Controls are immutable once a provider call has entered the recording stream.
	if len(host.calls) != 0 || host.determinism.Valid() {
		return ErrCapabilityReplayInvalid
	}
	host.determinism = control
	return nil
}

// canonicalReplayValue preserves exact JSON meaning while rejecting ambiguous or oversized material.
func canonicalReplayValue(raw json.RawMessage, maxBytes int) (json.RawMessage, error) {
	// A missing successful response is recorded as JSON null, matching the worker bridge.
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if len(raw) > maxBytes {
		return nil, ErrCapabilityReplayInvalid
	}
	canonical, err := canonicaljson.Canonicalize(raw)
	if err != nil || len(canonical) > maxBytes {
		return nil, ErrCapabilityReplayInvalid
	}
	return canonical, nil
}

// Fetch records deterministic outcomes and exposes credential repair guidance without provider payloads.
func (host *recordingCapabilityHost) Fetch(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	// A missing live host cannot become a replay-only source of provider authority.
	if host == nil || host.base == nil {
		return nil, ErrCapabilityReplayInvalid
	}
	request, err := canonicalReplayValue(raw, store.MaxReplayEvidenceBytes)
	if err != nil {
		return nil, err
	}
	host.mu.Lock()
	// The recorder must stop before dispatch once the replayable call budget is exhausted.
	if len(host.calls) >= 32 {
		host.mu.Unlock()
		return nil, ErrCapabilityReplayInvalid
	}
	index := len(host.calls)
	ordinal, ok := sandbox.CapabilityCallOrdinal(ctx)
	// Direct host tests have no interpreter sequence, while worker calls carry their authored order.
	if !ok {
		ordinal = index + 1
	}
	host.calls = append(host.calls, capabilityReplayCall{Ordinal: ordinal, Request: request})
	host.mu.Unlock()
	response, fetchErr := host.base.Fetch(ctx, request)
	// Raw provider errors stay private; Engine-owned credential guidance is safe for authored error handling.
	if fetchErr != nil {
		host.mu.Lock()
		// Allocate private evidence only for failed calls, outside the replay transcript.
		if host.diagnosticErrors == nil {
			host.diagnosticErrors = make(map[int]*executionappvm.DiagnosticError)
		}
		host.diagnosticErrors[index] = executionappvm.PrivateDiagnostic(fetchErr)
		host.mu.Unlock()
		failure := capabilityRecordedFetchError
		var missing *sandbox.CredentialMaterialMissingError
		// Credential absence contains metadata and a repair command, never credential values.
		if errors.As(fetchErr, &missing) {
			failure = errors.New(executionappvm.BoundDiagnostic(missing.Error()))
		}
		host.finishFetch(index, nil, failure.Error())
		return nil, failure
	}
	canonical, err := canonicalReplayValue(response, store.MaxReplayEvidenceBytes)
	if err != nil {
		host.mu.Lock()
		host.invalid = true
		host.mu.Unlock()
		return nil, err
	}
	host.finishFetch(index, canonical, "")
	return canonical, nil
}

// finishFetch saves the result's completion ordinal independently of invocation order.
func (host *recordingCapabilityHost) finishFetch(index int, response json.RawMessage, message string) {
	host.mu.Lock()
	defer host.mu.Unlock()
	host.completed++
	host.calls[index].Response = response
	host.calls[index].Error = message
	host.calls[index].Completion = host.completed
}

// DBGet delegates live storage and tracks the document seen by authored code.
func (host *recordingCapabilityHost) DBGet(ctx context.Context) (json.RawMessage, error) {
	if host == nil || host.base == nil {
		return nil, ErrCapabilityReplayInvalid
	}
	// Serializing per-execution DB calls makes the live snapshot match the replay overlay order.
	host.dbMu.Lock()
	defer host.dbMu.Unlock()
	data, err := host.base.DBGet(ctx)
	if err != nil {
		return nil, err
	}
	canonical, err := canonicalReplayValue(data, store.MaxExecutionDataBytes)
	if err != nil {
		return nil, err
	}
	host.mu.Lock()
	host.data = canonical
	host.mu.Unlock()
	return canonical, nil
}

// DBSet delegates live storage before updating the recorder's document snapshot.
func (host *recordingCapabilityHost) DBSet(ctx context.Context, raw json.RawMessage) error {
	if host == nil || host.base == nil {
		return ErrCapabilityReplayInvalid
	}
	// A set and a concurrent get must observe one total document order.
	host.dbMu.Lock()
	defer host.dbMu.Unlock()
	data, err := canonicalReplayValue(raw, store.MaxExecutionDataBytes)
	if err != nil {
		return err
	}
	if err := host.base.DBSet(ctx, data); err != nil {
		return err
	}
	host.mu.Lock()
	host.data = data
	host.mu.Unlock()
	return nil
}

// History serializes only canonical requests, successful results, and generic failures.
func (host *recordingCapabilityHost) History() (json.RawMessage, error) {
	if host == nil {
		return nil, ErrCapabilityReplayInvalid
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.invalid || host.completed != len(host.calls) || !host.determinism.Valid() {
		return nil, ErrCapabilityReplayInvalid
	}
	// Concurrent Go dispatch cannot define authored order; the interpreter's ordinals do.
	sort.Slice(host.calls, func(i, j int) bool { return host.calls[i].Ordinal < host.calls[j].Ordinal })
	for index, call := range host.calls {
		// Duplicate ordinals make the transcript ambiguous even when every provider call finished.
		if index > 0 && call.Ordinal == host.calls[index-1].Ordinal {
			return nil, ErrCapabilityReplayInvalid
		}
	}
	raw, err := json.Marshal(capabilityReplayHistory{Version: 2, Determinism: host.determinism, Calls: host.calls})
	if err != nil {
		return nil, ErrCapabilityReplayInvalid
	}
	return canonicalReplayValue(raw, store.MaxReplayEvidenceBytes)
}

// Data returns the live document snapshot for optional replay result comparison.
func (host *recordingCapabilityHost) Data() json.RawMessage {
	if host == nil {
		return nil
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	return bytes.Clone(host.data)
}

// NewReplayCapabilityHost validates one immutable evidence stream before running sandbox code.
func NewReplayCapabilityHost(raw json.RawMessage) (*replayCapabilityHost, error) {
	canonical, err := canonicalReplayValue(raw, store.MaxReplayEvidenceBytes)
	if err != nil {
		return nil, err
	}
	var history capabilityReplayHistory
	if err := json.Unmarshal(canonical, &history); err != nil || history.Version != 2 || !history.Determinism.Valid() || len(history.Calls) > 32 {
		return nil, ErrCapabilityReplayInvalid
	}
	if err := validateReplayCalls(history.Calls); err != nil {
		return nil, err
	}
	host := &replayCapabilityHost{calls: history.Calls, data: json.RawMessage("null"), completion: make([]chan struct{}, len(history.Calls)), consumed: make(map[int]struct{}, len(history.Calls)), determinism: history.Determinism}
	for index := range host.completion {
		host.completion[index] = make(chan struct{})
	}
	return host, nil
}

// Determinism returns the immutable worker controls recovered from encrypted replay evidence.
func (host *replayCapabilityHost) Determinism() sandbox.CapabilityDeterminism {
	// Nil hosts cannot supply controls to a replay worker.
	if host == nil {
		return sandbox.CapabilityDeterminism{}
	}
	return host.determinism
}

// validateReplayCalls requires a complete permutation of completion ordinals and canonical call JSON.
func validateReplayCalls(calls []capabilityReplayCall) error {
	seen := make(map[int]struct{}, len(calls))
	for index, call := range calls {
		if err := validateReplayCall(call, len(calls)); err != nil {
			return ErrCapabilityReplayInvalid
		}
		// Call ordinals must follow authored order and may contain gaps for DB calls.
		if call.Ordinal < 1 || (index > 0 && call.Ordinal <= calls[index-1].Ordinal) {
			return ErrCapabilityReplayInvalid
		}
		// Completion ordinals are a permutation, so concurrent results cannot be delivered twice.
		if _, exists := seen[call.Completion]; exists {
			return ErrCapabilityReplayInvalid
		}
		seen[call.Completion] = struct{}{}
	}
	return nil
}

// validateReplayCall admits exact invocations and the same bounded failures visible during the live run.
func validateReplayCall(call capabilityReplayCall, count int) error {
	// An incomplete or contradictory call cannot support deterministic side-effect-free execution.
	if !validReplayCallShape(call, count) {
		return ErrCapabilityReplayInvalid
	}
	if _, err := canonicalReplayValue(call.Request, store.MaxReplayEvidenceBytes); err != nil {
		return ErrCapabilityReplayInvalid
	}
	if call.Error != "" {
		// Only Engine-owned credential recovery messages extend the original generic error contract.
		if call.Error != capabilityRecordedFetchError.Error() && (!strings.HasPrefix(call.Error, "bucket_credentials_missing:") || len(call.Error) > 65536) {
			return ErrCapabilityReplayInvalid
		}
		return nil
	}
	if len(call.Response) == 0 {
		return ErrCapabilityReplayInvalid
	}
	if _, err := canonicalReplayValue(call.Response, store.MaxReplayEvidenceBytes); err != nil {
		return ErrCapabilityReplayInvalid
	}
	return nil
}

// validReplayCallShape checks the small structural invariant before JSON validation.
func validReplayCallShape(call capabilityReplayCall, count int) bool {
	return call.Completion >= 1 && call.Completion <= count && len(call.Request) > 0 && (call.Error == "" || call.Response == nil)
}

// Fetch matches the next recorded call and releases its result in original completion order.
func (host *replayCapabilityHost) Fetch(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	request, err := canonicalReplayValue(raw, store.MaxReplayEvidenceBytes)
	if err != nil {
		return nil, err
	}
	call, err := host.claimReplayCall(ctx, request)
	if err != nil {
		return nil, err
	}
	// Parallel calls receive results in the recorded completion order, preserving Promise behavior.
	if call.Completion > 1 {
		select {
		case <-host.completion[call.Completion-2]:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	close(host.completion[call.Completion-1])
	if call.Error != "" {
		// Preserve the legacy sentinel while replaying actionable messages exactly as authored code saw them.
		if call.Error == capabilityRecordedFetchError.Error() {
			return nil, capabilityRecordedFetchError
		}
		return nil, errors.New(call.Error)
	}
	return bytes.Clone(call.Response), nil
}

// claimReplayCall matches a canonical request to its authored ordinal before releasing any result.
func (host *replayCapabilityHost) claimReplayCall(ctx context.Context, request json.RawMessage) (capabilityReplayCall, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	ordinal, sequenced := sandbox.CapabilityCallOrdinal(ctx)
	index := host.nextCall
	// A worker call must match its authored ordinal even when Go dispatch races other calls.
	if sequenced {
		index = sort.Search(len(host.calls), func(i int) bool { return host.calls[i].Ordinal >= ordinal })
	}
	// Replay never falls back to a live host when an authored call diverges.
	if host.diverged || index >= len(host.calls) || (sequenced && host.calls[index].Ordinal != ordinal) || !bytes.Equal(request, host.calls[index].Request) {
		host.diverged = true
		return capabilityReplayCall{}, ErrCapabilityReplayDiverged
	}
	call := host.calls[index]
	// Duplicate worker deliveries cannot consume one recorded provider result twice.
	if _, exists := host.consumed[call.Ordinal]; exists {
		host.diverged = true
		return capabilityReplayCall{}, ErrCapabilityReplayDiverged
	}
	host.consumed[call.Ordinal] = struct{}{}
	host.nextCall++
	return call, nil
}

// DBGet reads only the temporary replay document without touching Engine storage.
func (host *replayCapabilityHost) DBGet(context.Context) (json.RawMessage, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	return bytes.Clone(host.data), nil
}

// DBSet replaces only the temporary replay document and enforces the live size bound.
func (host *replayCapabilityHost) DBSet(_ context.Context, raw json.RawMessage) error {
	data, err := canonicalReplayValue(raw, store.MaxExecutionDataBytes)
	if err != nil {
		return err
	}
	host.mu.Lock()
	host.data = data
	host.mu.Unlock()
	return nil
}

// Data returns the temporary document for comparison and never persists it.
func (host *replayCapabilityHost) Data() json.RawMessage {
	host.mu.Lock()
	defer host.mu.Unlock()
	return bytes.Clone(host.data)
}

// Consumed proves the authored script matched every recorded call and completion.
func (host *replayCapabilityHost) Consumed() error {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.diverged || host.nextCall != len(host.calls) {
		return ErrCapabilityReplayDiverged
	}
	for _, completion := range host.completion {
		// An invoked call that has not delivered its result is incomplete evidence use.
		select {
		case <-completion:
		default:
			return ErrCapabilityReplayDiverged
		}
	}
	return nil
}

var _ sandbox.CapabilityScriptHost = (*recordingCapabilityHost)(nil)
var _ sandbox.CapabilityScriptHost = (*replayCapabilityHost)(nil)
