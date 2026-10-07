package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
)

const maxUnifiedAppCallDepth = 8

type unifiedAppCallKey struct{}

type unifiedAppCallBudget struct {
	mu        sync.Mutex
	remaining int
}

type unifiedAppCallChain struct {
	families []uuid.UUID
	budget   *unifiedAppCallBudget
}

// withUnifiedAppCallRoot gives every live run one bounded tree of nested calls, including parallel branches.
func withUnifiedAppCallRoot(ctx context.Context, family uuid.UUID) context.Context {
	// Descendants retain their original root budget rather than resetting it per worker.
	if _, ok := ctx.Value(unifiedAppCallKey{}).(unifiedAppCallChain); ok {
		return ctx
	}
	return context.WithValue(ctx, unifiedAppCallKey{}, unifiedAppCallChain{
		families: []uuid.UUID{family}, budget: &unifiedAppCallBudget{remaining: maxCapabilityFetchCalls},
	})
}

// descendUnifiedAppCall rejects recursive families and excessive fan-out before starting another worker.
func descendUnifiedAppCall(ctx context.Context, caller, target uuid.UUID) (context.Context, error) {
	ctx = withUnifiedAppCallRoot(ctx, caller)
	chain := ctx.Value(unifiedAppCallKey{}).(unifiedAppCallChain)
	// The root counts toward depth, bounding retained parent interpreters and stack metadata.
	if len(chain.families) >= maxUnifiedAppCallDepth {
		return nil, errors.New("Unified App call depth exceeded")
	}
	for _, family := range chain.families {
		// Family identity also rejects a call back into a different version of an active parent.
		if family == target {
			return nil, errors.New("Unified App recursive call rejected")
		}
	}
	chain.budget.mu.Lock()
	defer chain.budget.mu.Unlock()
	// One shared counter prevents parallel branches from multiplying the total call budget.
	if chain.budget.remaining == 0 {
		return nil, errors.New("Unified App nested call limit exceeded")
	}
	chain.budget.remaining--
	families := append(append([]uuid.UUID{}, chain.families...), target)
	return context.WithValue(ctx, unifiedAppCallKey{}, unifiedAppCallChain{families: families, budget: chain.budget}), nil
}

// callUnifiedApp delegates only an authored alias and records its envelope through the ordinary fetch transcript.
func (host *executionCapabilityHost) callUnifiedApp(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Alias string         `json:"unifiedApp"`
		Input map[string]any `json:"input"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	// Provider selectors, IDs, credentials and absent inputs cannot cross the app-call boundary.
	if err := decoder.Decode(&request); err != nil || request.Input == nil || host.apps == nil {
		return nil, errors.New("Unified App call request is invalid")
	}
	var trailing any
	// A second JSON value must not conceal additional request controls.
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("Unified App call request is invalid")
	}
	host.mu.Lock()
	host.callCount++
	count := host.callCount
	host.mu.Unlock()
	// Hosted and provider calls share the existing per-invocation effect budget.
	if count > maxCapabilityFetchCalls {
		return nil, errors.New("Unified App operation limit exceeded")
	}
	input, err := json.Marshal(request.Input)
	// Invalid JSON cannot reach the child executor or become a recorded successful effect.
	if err != nil {
		return nil, err
	}
	host.mu.Lock()
	// A delegated child may perform provider effects even if the parent later fails or is cancelled.
	host.called = true
	host.mu.Unlock()
	return host.apps.ExecuteAttachedUnifiedApp(sandbox.WithNestedCapabilityCall(ctx), host.identity, request.Alias, input)
}
