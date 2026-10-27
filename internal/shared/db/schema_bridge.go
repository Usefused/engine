package db

// bridgeSchemaQueries keeps reverse-session state and durable ingress in the Engine database.
func bridgeSchemaQueries() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS fused_bridge_pairings (
 id uuid PRIMARY KEY, account_id uuid NOT NULL, workspace_id uuid NOT NULL REFERENCES fused_workspaces(id),
 subject_id uuid NOT NULL REFERENCES fused_subjects(id), installation_id uuid NOT NULL, destination_id uuid NOT NULL UNIQUE,
 name text NOT NULL, credential_hash text NOT NULL UNIQUE, profile_ids text[] NOT NULL DEFAULT '{}',
 generation bigint NOT NULL DEFAULT 0, owner_instance text NOT NULL DEFAULT '', lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz
 )`,
		`CREATE TABLE IF NOT EXISTS fused_bridge_manifests (
 pairing_id uuid NOT NULL REFERENCES fused_bridge_pairings(id), profile_id text NOT NULL,
 revision text NOT NULL, hash text NOT NULL, document bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(pairing_id,profile_id,revision),
 UNIQUE(pairing_id,profile_id,hash)
 )`,
		`CREATE TABLE IF NOT EXISTS fused_bridge_profiles (
 pairing_id uuid NOT NULL REFERENCES fused_bridge_pairings(id), profile_id text NOT NULL, manifest_hash text NOT NULL,
 PRIMARY KEY(pairing_id,profile_id), FOREIGN KEY(pairing_id,profile_id,manifest_hash)
 REFERENCES fused_bridge_manifests(pairing_id,profile_id,hash)
 )`,
		`CREATE TABLE IF NOT EXISTS fused_bridge_events (
 pairing_id uuid NOT NULL REFERENCES fused_bridge_pairings(id), destination_id uuid NOT NULL, event_id text NOT NULL,
 profile_id text NOT NULL, manifest_hash text NOT NULL, content_hash text NOT NULL, envelope bytea NOT NULL,
 accepted_at timestamptz NOT NULL DEFAULT now(), delivered_at timestamptz,
 PRIMARY KEY(pairing_id,destination_id,event_id),
 FOREIGN KEY(pairing_id,profile_id,manifest_hash) REFERENCES fused_bridge_manifests(pairing_id,profile_id,hash)
 )`,
		// Historical consent stays narrow until a new OAuth workspace pairing is created.
		`ALTER TABLE fused_bridge_pairings ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'profiles'`,
		`ALTER TABLE fused_bridge_pairings ADD COLUMN IF NOT EXISTS inventory_generation bigint NOT NULL DEFAULT -1`,
		// Inventory includes profiles whose contracts have never been resolved.
		`ALTER TABLE fused_bridge_profiles ALTER COLUMN manifest_hash DROP NOT NULL`,
		`ALTER TABLE fused_bridge_profiles ADD COLUMN IF NOT EXISTS state text NOT NULL DEFAULT 'online'`,
		`CREATE INDEX IF NOT EXISTS fused_bridge_events_pending ON fused_bridge_events(accepted_at) WHERE delivered_at IS NULL`,
	}
}
