package api

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"go.opentelemetry.io/otel"
)

type bufferedCapabilityHost struct {
	base sandbox.CapabilityScriptHost
	mu   sync.Mutex
	data json.RawMessage
}

// NewBufferedCapabilityHost keeps one execution's JSONB document local until terminal persistence.
func NewBufferedCapabilityHost(base sandbox.CapabilityScriptHost) *bufferedCapabilityHost {
	return &bufferedCapabilityHost{base: base, data: json.RawMessage("null")}
}

// Fetch delegates only selected provider work while DB effects remain buffered.
func (host *bufferedCapabilityHost) Fetch(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	// An absent live bridge cannot become a source of provider authority.
	if host == nil || host.base == nil {
		return nil, ErrCapabilityReplayInvalid
	}
	return host.base.Fetch(ctx, request)
}

// DBGet reads the most recent local write and never queries an intermediate SQL row.
func (host *bufferedCapabilityHost) DBGet(context.Context) (json.RawMessage, error) {
	if host == nil {
		return nil, ErrCapabilityReplayInvalid
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	return bytes.Clone(host.data), nil
}

// DBSet validates and replaces the local document without making a durable write.
func (host *bufferedCapabilityHost) DBSet(ctx context.Context, raw json.RawMessage) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_data.buffer.set")
	defer span.End()
	if host == nil {
		return ErrCapabilityReplayInvalid
	}
	canonical, err := canonicaljson.Canonicalize(raw)
	// Malformed JSON cannot become the execution's terminal document.
	if err != nil {
		return store.ErrExecutionResultInvalid
	}
	// The same canonical byte limit applies before the terminal SQL update.
	if len(canonical) > store.MaxExecutionDataBytes {
		return store.ErrExecutionResultDataTooLarge
	}
	host.mu.Lock()
	host.data = canonical
	host.mu.Unlock()
	return nil
}

// Data returns a copy of the final document for one atomic terminal update.
func (host *bufferedCapabilityHost) Data() json.RawMessage {
	if host == nil {
		return nil
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	return bytes.Clone(host.data)
}

var _ sandbox.CapabilityScriptHost = (*bufferedCapabilityHost)(nil)
