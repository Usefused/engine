package bridgehost

import (
	"context"
	"encoding/json"
	"sort"

	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
)

// validateInventory binds a bounded complete Profile set to a deterministic cross-language digest.
func validateInventory(frame *bridgev1.Inventory) ([]string, error) {
	// Empty inventories withdraw every profile; a missing frame is not an empty inventory.
	if frame == nil || len(frame.ProfileIds) > 1000 {
		return nil, ErrContract
	}
	profiles := append([]string{}, frame.ProfileIds...)
	sort.Strings(profiles)
	for i, id := range profiles {
		// Duplicate or malformed identities make authoritative reconciliation ambiguous.
		if !identifier.MatchString(id) || (i > 0 && profiles[i-1] == id) {
			return nil, ErrContract
		}
	}
	raw, _ := json.Marshal(profiles)
	// Hash the compact sorted JSON array, including [] for an empty workspace.
	if Digest(raw) != frame.Hash {
		return nil, ErrContract
	}
	return profiles, nil
}

// PutInventory atomically withdraws missing profiles before permitting this generation to publish work.
func (r *Repository) PutInventory(ctx context.Context, p Pairing, frame *bridgev1.Inventory) error {
	profiles, err := validateInventory(frame)
	// Invalid inventories must leave the existing catalogue unchanged.
	if err != nil {
		return err
	}
	tx, err := r.Pool.Begin(ctx)
	// Catalogue changes and generation readiness share one commit boundary.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var reset bool
	err = tx.QueryRow(ctx, `SELECT inventory_generation<>generation FROM fused_bridge_pairings p WHERE id=$1 AND generation=$2 AND scope='workspace' AND revoked_at IS NULL AND lease_until>now() AND `+ownerAuthorized+` FOR UPDATE`, p.ID, p.Generation).Scan(&reset)
	// Legacy consent and replaced sessions cannot publish a workspace-wide inventory.
	if err != nil {
		return ErrDenied
	}
	_, err = tx.Exec(ctx, `UPDATE fused_bridge_profiles SET state='removed' WHERE pairing_id=$1 AND NOT(profile_id=ANY($2::text[]))`, p.ID, profiles)
	// A failed withdrawal must not acknowledge a partially reconciled workspace.
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO fused_bridge_profiles(pairing_id,profile_id,state) SELECT $1,unnest($2::text[]),'offline' ON CONFLICT(pairing_id,profile_id) DO UPDATE SET state=CASE WHEN $3 OR fused_bridge_profiles.state='removed' THEN 'offline' ELSE fused_bridge_profiles.state END`, p.ID, profiles, reset)
	// A new generation must refresh live status before old online contracts can execute.
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE fused_bridge_pairings SET inventory_generation=generation WHERE id=$1`, p.ID)
	// Readiness cannot outlive failure to persist the full inventory.
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
