// Cross-account public-browsing route: /integrations/<provider>/<slug>.
//
// account_scoped_service_slugs_plan.md's URL model makes service slugs
// unique only within an account, so a service belonging to an account
// other than the caller's own needs its owning account's `provider`
// segment to disambiguate. This route matches that two-segment shape and
// hands off to the exact same loader/meta/component integrations.$id.tsx
// already has -- that loader reads params.provider (undefined on the
// single-segment route, present here) and threads it into the
// service/serviceVersions GraphQL queries via resolveAccountScopedService
// on the backend.
//
// Reusing the same component keeps public catalogue browsing and the
// provider-aware URL model consistent with the caller's own service view.
export { clientLoader, meta, default } from "./integrations.$id";
