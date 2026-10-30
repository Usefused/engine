package store

// appliedAppDescriptionSQL repairs presentation from the same immutable plan without
// rewriting generation inputs or borrowing another version's authored description.
// Only the exact historical generated placeholder is replaced; custom text survives.
const appliedAppDescriptionSQL = `CASE
 WHEN applied.resolved_payload->>'description' = 'GitOps managed SDK ' || (applied.resolved_payload->>'name')
 THEN COALESCE(BTRIM(applied.desired_state->>'description'), '')
 ELSE COALESCE(applied.resolved_payload->>'description', '')
 END`
