package mcpskill

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	// polishTimeout bounds the optional AI step so planning never waits on a slow model.
	polishTimeout = 20 * time.Second
	// polishOperationLimit bounds how many operation IDs are sent as evidence.
	polishOperationLimit = 200
	// polishDescriptionLimit bounds the authored description sent as evidence.
	polishDescriptionLimit = 2000
)

// PolishService is one service and its operation IDs, sent to the Registry as evidence only.
type PolishService struct {
	Label      string   `json:"label"`
	Operations []string `json:"operations"`
}

// PolishRequest carries public version metadata only: no credentials, URLs, schemas or tokens.
type PolishRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Services    []PolishService `json:"services"`
	Other       []string        `json:"other,omitempty"`
}

// Polisher asks the licensed Registry LLM to rewrite the skill's prose slots.
type Polisher interface {
	PolishMCPSkill(context.Context, PolishRequest) (Prose, error)
}

var (
	polisherMu sync.RWMutex
	polisher   Polisher
)

// SetPolisher wires the Registry client at process startup; nil disables AI prose.
func SetPolisher(client Polisher) {
	polisherMu.Lock()
	defer polisherMu.Unlock()
	polisher = client
}

// NewPolishRequest builds bounded evidence from a snapshot without mutating it.
func NewPolishRequest(name, description string, snapshot *Snapshot) PolishRequest {
	request := PolishRequest{Name: Text(name), Description: truncate(Text(description), polishDescriptionLimit), Services: []PolishService{}}
	// A version without a snapshot still has a name and description worth polishing.
	if snapshot == nil {
		return request
	}
	remaining := polishOperationLimit
	for _, group := range snapshot.Services {
		operations := append(append([]string(nil), group.Operations...), group.Imported...)
		// The shared budget keeps the prompt bounded however large the catalogue is.
		if len(operations) > remaining {
			operations = operations[:remaining]
		}
		remaining -= len(operations)
		request.Services = append(request.Services, PolishService{Label: group.Label, Operations: operations})
	}
	other := snapshot.Other
	// Operations without a service share the same evidence budget.
	if len(other) > remaining {
		other = other[:remaining]
	}
	request.Other = append([]string(nil), other...)
	return request
}

// Polish returns validated AI prose, or nil when AI is unavailable, slow, or returns anything unsafe.
// It never fails the caller: the deterministic template is always a complete skill.
func Polish(ctx context.Context, request PolishRequest) *Prose {
	polisherMu.RLock()
	client := polisher
	polisherMu.RUnlock()
	// Without a configured Registry client the template is the intended result, not an error.
	if client == nil {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, polishTimeout)
	defer cancel()
	prose, err := client.PolishMCPSkill(bounded, request)
	// Model or transport failures are logged without content and fall back to the template.
	if err != nil {
		slog.InfoContext(ctx, "MCP skill prose polish unavailable", "reason", err.Error())
		return nil
	}
	admitted, err := ValidateProse(prose)
	// Prose is re-validated locally so the Registry is not trusted with document structure.
	if err != nil {
		slog.InfoContext(ctx, "MCP skill prose polish rejected", "reason", err.Error())
		return nil
	}
	// An empty answer adds nothing over the template.
	if strings.TrimSpace(admitted.Purpose) == "" && len(admitted.WhenToUse) == 0 {
		return nil
	}
	return &admitted
}
