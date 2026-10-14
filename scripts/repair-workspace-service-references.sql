-- Run against the Engine database after deploying provider-qualified activation:
-- psql "$ENGINE_DATABASE_URL" -X -v ON_ERROR_STOP=1 -f scripts/repair-workspace-service-references.sql
-- Only slug metadata changes. Service IDs, version pins, snapshots, credentials,
-- app selections, and ownership remain untouched. The transaction is repeatable.
BEGIN;
SET LOCAL lock_timeout = '10s';

-- Serialize membership and snapshot changes while checking provider evidence.
LOCK TABLE fused_workspace_services IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE fused_workspace_service_versions, fused_service_contract_snapshots IN SHARE MODE;

CREATE TEMP TABLE fused_service_reference_repair ON COMMIT DROP AS
SELECT service.service_id, service.service_slug AS old_slug,
       count(DISTINCT NULLIF(snapshot.service_metadata #>> '{provider,handle}', '')) AS provider_count,
       '@' || min(NULLIF(snapshot.service_metadata #>> '{provider,handle}', '')) || '/' || service.service_slug AS canonical_ref
FROM fused_workspace_services service
LEFT JOIN fused_workspace_service_versions version ON version.service_id = service.service_id
LEFT JOIN fused_service_contract_snapshots snapshot
  ON snapshot.service_id = version.service_id AND snapshot.service_version_id = version.service_version_id
WHERE service.service_slug IS NOT NULL AND service.service_slug NOT LIKE '@%'
GROUP BY service.service_id, service.service_slug;

-- Missing or contradictory provider evidence requires a Registry-confirmed repair,
-- never a guessed provider, arbitrary row choice, or removal of service identity.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM fused_service_reference_repair
               WHERE provider_count <> 1 OR canonical_ref !~ '^@[^/@[:space:]]+/[^/@[:space:]]+$') THEN
        RAISE EXCEPTION 'Provider reference repair aborted: refresh missing or conflicting Registry provider metadata before retrying';
    END IF;
    -- A destination already claimed by another ID is a separate identity conflict.
    IF EXISTS (
        SELECT 1 FROM fused_service_reference_repair repair
        JOIN fused_workspace_services existing ON lower(existing.service_slug) = lower(repair.canonical_ref)
        WHERE existing.service_id <> repair.service_id
    ) OR EXISTS (
        SELECT lower(canonical_ref) FROM fused_service_reference_repair
        GROUP BY lower(canonical_ref) HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'Provider reference repair aborted: multiple service IDs claim the same provider-qualified reference';
    END IF;
END $$;

-- Match by immutable ID; never transfer an existing slug owner to another service.
UPDATE fused_workspace_services service
SET service_slug = repair.canonical_ref
FROM fused_service_reference_repair repair
WHERE service.service_id = repair.service_id
RETURNING service.service_id, repair.old_slug, service.service_slug AS canonical_ref;

COMMIT;
