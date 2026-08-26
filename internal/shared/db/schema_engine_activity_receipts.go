package db

// activityReceiptMigrationQueries extends canonical history without rewriting old receipts or lifecycle events.
func activityReceiptMigrationQueries() []string {
	return []string{
		`ALTER TABLE fused_engine_execution_events
		 ADD COLUMN IF NOT EXISTS execution_kind text NOT NULL DEFAULT 'physical' CHECK (execution_kind IN ('physical','unified')),
		 ADD COLUMN IF NOT EXISTS parent_execution_id uuid,
		 ADD COLUMN IF NOT EXISTS unified_target text NOT NULL DEFAULT '',
		 ADD COLUMN IF NOT EXISTS execution_phase text NOT NULL DEFAULT '',
		 ADD COLUMN IF NOT EXISTS unified_steps jsonb NOT NULL DEFAULT '[]';`,
		`CREATE INDEX IF NOT EXISTS idx_fused_execution_parent ON fused_engine_execution_events(account_id, app_family_id, parent_execution_id, started_at, id) WHERE parent_execution_id IS NOT NULL;`,
		`ALTER TABLE fused_mcp_sessions
		 ADD COLUMN IF NOT EXISTS client_name text NOT NULL DEFAULT '' CHECK (octet_length(client_name) <= 128),
		 ADD COLUMN IF NOT EXISTS client_version text NOT NULL DEFAULT '' CHECK (octet_length(client_version) <= 128),
		 ADD COLUMN IF NOT EXISTS initial_client_ip inet;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_sessions_cursor ON fused_mcp_sessions(app_id, started_at DESC, id DESC);`,
	}
}
