package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type bridgeGroupCatalogue struct {
	Pairing, Hash string
	Document      []byte
}

// Resolve names only inside the caller's workspace; sibling workspaces are not candidates.
func (s *postgresStore) bridgeGroupCatalogue(ctx context.Context, account uuid.UUID, ref models.BridgeReference) (bridgeGroupCatalogue, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok || actor.AccountID != account {
		return bridgeGroupCatalogue{}, errors.New("Bridge workspace authorization unavailable")
	}
	rows, err := s.db.Query(ctx, `SELECT p.id::text,COALESCE(current.manifest_hash,''),COALESCE(m.document,''::bytea)
 FROM fused_bridge_pairings p
 LEFT JOIN fused_bridge_profiles current ON current.pairing_id=p.id AND current.profile_id=$4 AND current.state<>'removed'
 LEFT JOIN fused_bridge_manifests m ON m.pairing_id=p.id AND m.profile_id=current.profile_id AND m.hash=current.manifest_hash
 WHERE p.account_id=$1 AND p.workspace_id=$2 AND p.name=$3 AND p.revoked_at IS NULL AND p.scope='workspace' LIMIT 2`, account, actor.WorkspaceID, ref.Bridge, ref.Profile)
	if err != nil {
		return bridgeGroupCatalogue{}, err
	}
	defer rows.Close()
	var matches []bridgeGroupCatalogue
	for rows.Next() {
		var c bridgeGroupCatalogue
		if err = rows.Scan(&c.Pairing, &c.Hash, &c.Document); err != nil {
			return c, err
		}
		matches = append(matches, c)
	}
	if err = rows.Err(); err != nil {
		return bridgeGroupCatalogue{}, err
	}
	if len(matches) == 0 {
		return bridgeGroupCatalogue{}, fmt.Errorf("Bridge %q not found in this workspace", ref.Bridge)
	}
	if len(matches) > 1 {
		return bridgeGroupCatalogue{}, fmt.Errorf("Bridge name %q is ambiguous; use distinct installation names", ref.Bridge)
	}
	if matches[0].Hash == "" {
		return bridgeGroupCatalogue{}, fmt.Errorf("profile %q has no available schema on Bridge %q", ref.Profile, ref.Bridge)
	}
	return matches[0], nil
}

// A deterministic internal alias keeps runtime authorization unchanged and collision checked.
func bridgeGroupAlias(group, connection, operation string) string {
	sum := sha256.Sum256([]byte(group + "\x00" + connection + "\x00" + operation))
	return "b" + hex.EncodeToString(sum[:])[:63]
}

func expandBridgeGroup(group string, ref models.BridgeReference, catalogue bridgeGroupCatalogue) (map[string]models.BridgeReference, error) {
	if err := models.ValidateBridgeGroup(ref); err != nil {
		return nil, err
	}
	var manifest struct {
		Operations []struct {
			Connection string `json:"connection_id"`
			Operation  string `json:"operation_id"`
			Contract   string `json:"contract_json"`
		} `json:"operations"`
	}
	if json.Unmarshal(catalogue.Document, &manifest) != nil {
		return nil, errors.New("invalid Bridge manifest")
	}
	available := map[string]map[string]string{}
	for _, op := range manifest.Operations {
		var contract struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(op.Contract), &contract) != nil {
			return nil, errors.New("invalid Bridge contract")
		}
		if available[op.Connection] == nil {
			available[op.Connection] = map[string]string{}
		}
		if _, exists := available[op.Connection][op.Operation]; exists {
			return nil, errors.New("duplicate Bridge manifest operation")
		}
		available[op.Connection][op.Operation] = contract.Kind
	}
	result := map[string]models.BridgeReference{}
	for connection, selection := range ref.Connections {
		ops, exists := available[connection]
		if !exists {
			return nil, fmt.Errorf("connection %q not found in profile %q", connection, ref.Profile)
		}
		selected, all, err := models.DecodeBridgeOperations(selection.Operations)
		if err != nil {
			return nil, err
		}
		if all {
			for op, kind := range ops {
				if kind == "read" || kind == "command" {
					selected = append(selected, op)
				}
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("connection %q has no callable operations", connection)
		}
		sort.Strings(selected)
		for _, op := range selected {
			kind, ok := ops[op]
			if !ok {
				return nil, fmt.Errorf("operation %q not found on connection %q", op, connection)
			}
			if kind != "read" && kind != "command" {
				return nil, fmt.Errorf("operation %q is not callable; events require explicit SDK event bindings", op)
			}
			if strings.Contains(op, "..") || strings.HasSuffix(op, ".") || len(strings.Split(op, ".")) > 16 {
				return nil, fmt.Errorf("operation %q cannot form a typed method path", op)
			}
			for _, parent := range selected {
				if strings.HasPrefix(op, parent+".") {
					return nil, fmt.Errorf("operations %q and %q have conflicting method paths", parent, op)
				}
			}
			alias := bridgeGroupAlias(group, connection, op)
			result[alias] = models.BridgeReference{PairingID: catalogue.Pairing, ProfileID: ref.Profile, ManifestHash: catalogue.Hash, ConnectionID: connection, OperationID: op}
			if len(result) > 100 {
				return nil, errors.New("at most 100 Bridge capabilities may be attached after wildcard expansion")
			}
		}
	}
	return result, nil
}

func (s *postgresStore) expandBridgeGroups(ctx context.Context, account uuid.UUID, refs map[string]models.BridgeReference) (map[string]models.BridgeReference, map[string]string, error) {
	expanded := map[string]models.BridgeReference{}
	groups := map[string]string{}
	for alias, ref := range refs {
		if !ref.Grouped() {
			expanded[alias] = ref
		}
	}
	for group, ref := range refs {
		if !ref.Grouped() {
			continue
		}
		catalogue, err := s.bridgeGroupCatalogue(ctx, account, ref)
		if err != nil {
			return nil, nil, err
		}
		pins, err := expandBridgeGroup(group, ref, catalogue)
		if err != nil {
			return nil, nil, err
		}
		for alias, pin := range pins {
			if _, exists := expanded[alias]; exists {
				return nil, nil, errors.New("conflicting generated Bridge aliases")
			}
			expanded[alias] = pin
			groups[alias] = group
		}
		if len(expanded) > 100 {
			return nil, nil, errors.New("at most 100 Bridge capabilities may be attached after wildcard expansion")
		}
	}
	return expanded, groups, nil
}
