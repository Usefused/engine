# Repair provider-qualified workspace service references

Registry allows separate accounts to own services with the same slug. Engine must retain the canonical `@provider/service` reference when activating each service. Older releases stripped the provider, so uploading another account's `plunk` could collide with an existing workspace service.

Deploy the Registry change that returns provider identity in the owned `services` query first, then the Engine change that persists canonical references. Run the repair against each affected **Engine database**, using an existing database connection:

```sh
psql "$ENGINE_DATABASE_URL" -X -v ON_ERROR_STOP=1 -f scripts/repair-workspace-service-references.sql
```

The transaction derives the provider from Engine contract snapshots associated with each service's existing version IDs, converts bare slugs into qualified references, and returns every changed row. Service IDs, version pins, credentials, and app selections remain unchanged. Repeating the script is harmless.

The script aborts without changes if a bare slug has missing or conflicting provider evidence, or another service ID already claims the destination reference. Those cases need authoritative Registry identity checked by service ID before repair; never assign a provider based only on the shared name.

Restart the Engine after repair so owned-service reconciliation can finish activation. Use `@provider/plunk` when selecting a service if multiple providers have Plunk enabled: a bare `plunk` reference is ambiguous. Existing webhook registrations can use their unique ingress slugs when a bare service alias becomes ambiguous.
