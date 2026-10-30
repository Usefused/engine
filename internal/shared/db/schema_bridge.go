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
		// Preserve consent provenance independently of short-lived OAuth access tokens.
		`ALTER TABLE fused_bridge_pairings ADD COLUMN IF NOT EXISTS oauth_client_id uuid`,
		`ALTER TABLE fused_bridge_pairings ADD COLUMN IF NOT EXISTS credential_source text NOT NULL DEFAULT 'legacy'`,
		`CREATE INDEX IF NOT EXISTS fused_bridge_pairings_consent ON fused_bridge_pairings(oauth_client_id,subject_id) WHERE revoked_at IS NULL`,
		// Commit-time notifications terminate idle sessions on every Engine replica.
		`CREATE OR REPLACE FUNCTION fused_notify_bridge_revocation() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
 PERFORM pg_notify('fused_bridge_revoked_' || md5(TG_TABLE_SCHEMA), NEW.id::text);
 RETURN NEW;
 END $$`,
		`CREATE OR REPLACE TRIGGER fused_bridge_revoked AFTER UPDATE OF revoked_at ON fused_bridge_pairings
 FOR EACH ROW WHEN (OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL)
 EXECUTE FUNCTION fused_notify_bridge_revocation()`,
		// Deleting consent fences all derived installations in the same transaction, including cascaded deletes.
		`CREATE OR REPLACE FUNCTION fused_revoke_consent_bridges() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
 UPDATE fused_bridge_pairings SET revoked_at=now(), generation=generation+1, lease_until=NULL
 WHERE oauth_client_id=OLD.client_id AND subject_id=OLD.subject_id AND revoked_at IS NULL;
 RETURN OLD;
 END $$`,
		`CREATE OR REPLACE TRIGGER fused_bridge_consent_deleted AFTER DELETE ON fused_oauth_user_consents
 FOR EACH ROW EXECUTE FUNCTION fused_revoke_consent_bridges()`,
		// Historical installations cannot be safely attributed to a consent and must pair again once.
		`UPDATE fused_bridge_pairings SET revoked_at=now(),generation=generation+1,lease_until=NULL
 WHERE credential_source='legacy' AND revoked_at IS NULL`,
		`ALTER TABLE fused_bridge_pairings ADD COLUMN IF NOT EXISTS inventory_generation bigint NOT NULL DEFAULT -1`,
		// Inventory includes profiles whose contracts have never been resolved.
		`ALTER TABLE fused_bridge_profiles ALTER COLUMN manifest_hash DROP NOT NULL`,
		`ALTER TABLE fused_bridge_profiles ADD COLUMN IF NOT EXISTS state text NOT NULL DEFAULT 'online'`,
		`CREATE INDEX IF NOT EXISTS fused_bridge_events_pending ON fused_bridge_events(accepted_at) WHERE delivered_at IS NULL`,
	}
}
