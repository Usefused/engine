package store

import (
	"fmt"

	"github.com/google/uuid"
)

// executionReceiptWhereClause distinguishes grouped app history from provider
// accounting. Analytics and service history default to physical receipts only.
func executionReceiptWhereClause(filter EngineExecutionFilter, where string, args []any) (string, []any) {
	// Child lookup is only meaningful inside an authorized app family, never workspace-wide.
	if filter.AppFamilyID != uuid.Nil && filter.ParentExecutionID != uuid.Nil {
		return where + fmt.Sprintf(" AND execution_kind = 'physical' AND parent_execution_id = $%d", len(args)+1), append(args, filter.ParentExecutionID)
	}
	// A child remains visible when its parent has not arrived, failed publication,
	// or expired first. SQL anti-join preserves evidence without per-row round trips.
	if filter.AppFamilyID != uuid.Nil && filter.ReceiptRoots {
		return where + ` AND (parent_execution_id IS NULL OR NOT EXISTS (
			SELECT 1 FROM fused_engine_execution_events parent
			WHERE parent.id = fused_engine_execution_events.parent_execution_id
			  AND parent.account_id = fused_engine_execution_events.account_id
			  AND parent.app_family_id = fused_engine_execution_events.app_family_id
			  AND parent.execution_kind = 'unified'))`, args
	}
	return where + " AND execution_kind = 'physical'", args
}
