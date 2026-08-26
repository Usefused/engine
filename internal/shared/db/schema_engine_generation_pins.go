package db

// generationContractPinMigrationQueries preserves existing runtime snapshots as
// unpinned; only a fresh Registry fetch can establish an archived generation identity.
func generationContractPinMigrationQueries() []string {
	return []string{`ALTER TABLE fused_service_contract_snapshots
		ADD COLUMN IF NOT EXISTS generation_contract_hash text NOT NULL DEFAULT ''
		CHECK (generation_contract_hash = '' OR generation_contract_hash ~ '^sha256:[0-9a-f]{64}$')`}
}
