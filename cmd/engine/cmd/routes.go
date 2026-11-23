package cmd

import (
	"github.com/go-chi/chi/v5"

	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/store"
)

// registerProxyRoutesWithRuntimeContracts mounts the Registry-proxied GraphQL
// and REST routes while keeping billing identity server-side after the shared
// Engine authorization boundary.
func registerProxyRoutesWithRuntimeContracts(r chi.Router, proxy api.Forwarder, s store.Store, contractFetcher api.RuntimeContractFetcher) {
	// Proxy routes authenticate locally, then RegistryProxy replaces inbound
	// auth with the Engine's licensed workspace identity.
	r.Post("/graphql", api.GraphQLProxyHandler(proxy, s))

	// Permission introspection is local; no browser credential is forwarded to Registry.
	r.Get("/account/billing/access", api.BillingAccessHandler)
	r.Get("/account/engine/access", api.HostedEngineAccessHandler(Version))
	restHandler := api.RESTProxyHandlerWithRuntimeContracts(proxy, s, contractFetcher)
	for _, path := range api.RESTProxyMountPaths {
		if path == "/leads" {
			// /leads is a single POST endpoint on the Registry, not a
			// sub-resource tree like the others -- Mount would also match
			// GET /leads and forward it to a Registry route that doesn't
			// exist there either, so this is POST-only on purpose.
			r.Post(path, restHandler)
			continue
		}
		r.Mount(path, restHandler)
	}
}
