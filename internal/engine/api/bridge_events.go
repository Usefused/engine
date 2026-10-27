package api

import (
	"context"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"strings"
)

// partitionBridgeEvents separates local aliases before existing provider webhook validation.
func partitionBridgeEvents(events []string) (local, other []string) {
	for _, event := range events {
		// Only the explicit local namespace can resolve an app-owned Bridge binding.
		if strings.Contains(event, ".bridge.") {
			local = append(local, event)
		} else {
			other = append(other, event)
		}
	}
	return
}

// bridgeEventSubjects resolves every requested alias from immutable app metadata and live installation authority.
func (s *EngineGRPCServer) bridgeEventSubjects(ctx context.Context, identity auth.RuntimeIdentity, requested []string) ([]string, error) {
	// Existing receivers do not require a Bridge host or storage adapter.
	if len(requested) == 0 {
		return nil, nil
	}
	repository, ok := s.store.(store.BridgeBindingStore)
	authority, active := s.bridgeHost.(interface {
		AuthorizeEventPin(context.Context, string, bridgehost.Pin) error
	})
	// Missing wiring cannot broaden an event subscription to an unverified broker filter.
	if !ok || !active {
		return nil, errors.New("Bridge event subscription unavailable")
	}
	bindings, err := repository.ReadBridgeBindings(ctx, identity.AccountID, identity.AppID)
	if err != nil {
		return nil, err
	}
	selected := selectedBridgeEvents(identity, bindings)
	subjects := []string{}
	for _, alias := range requested {
		pin, found := selected[alias]
		// Every requested alias must still be granted; partial success would conceal a revoked subscription.
		if !found {
			return nil, errors.New("Bridge event is not selected")
		}
		if err = authority.AuthorizeEventPin(ctx, identity.AccountID.String(), pin); err != nil {
			return nil, err
		}
		subjects = append(subjects, bridgehost.EventSubject(identity.AccountID.String(), pin))
	}
	return subjects, nil
}

// selectedBridgeEvents indexes only event pins granted by this exact token, never command aliases.
func selectedBridgeEvents(identity auth.RuntimeIdentity, bindings []models.BridgeBinding) map[string]bridgehost.Pin {
	selected := map[string]bridgehost.Pin{}
	for _, b := range bindings {
		// Event and command authority are distinct even when they share a connection.
		if b.Kind == "event" && identity.AllowsOperation("bridge:"+b.Alias) {
			pin := bridgehost.Pin{PairingID: b.PairingID, ProfileID: b.ProfileID, ManifestHash: b.ManifestHash, ConnectionID: b.ConnectionID, OperationID: b.OperationID, ContractHash: b.ContractHash}
			selected[b.PairingID+"."+bridgehost.EventName(pin)] = pin
		}
	}
	return selected
}
