package sandbox

import (
	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
)

// PaginationIntentFromProto converts the shared wire shape into the transport-independent runtime control.
func PaginationIntentFromProto(value *enginev1.PaginationIntent) (*engine.PaginationIntent, error) {
	// Message absence preserves automatic pagination and is distinct from a present zero value.
	if value == nil {
		return nil, nil
	}
	intent := &engine.PaginationIntent{MaxPages: int(value.GetMaxPages())}
	if err := engine.ValidatePaginationIntent(intent); err != nil {
		return nil, err
	}
	return intent, nil
}

// PaginationIntentsFromProto converts a Unified target map without retaining mutable protobuf messages.
func PaginationIntentsFromProto(values map[string]*enginev1.PaginationIntent) (map[string]*engine.PaginationIntent, error) {
	intents := make(map[string]*engine.PaginationIntent, len(values))
	// Each target value receives identical bounds before immutable operation resolution begins.
	for target, value := range values {
		intent, err := PaginationIntentFromProto(value)
		if err != nil || intent == nil {
			return nil, engine.ErrPaginationIntentInvalid
		}
		intents[target] = intent
	}
	return intents, nil
}
