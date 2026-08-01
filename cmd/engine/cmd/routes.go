package cmd

import (
	"github.com/go-chi/chi/v5"

	"github.com/Usefused/engine/internal/engine/api"
	enginemiddleware "github.com/Usefused/engine/internal/engine/middleware"
	"github.com/Usefused/engine/internal/engine/store"
)

// registerProxyRoutes mounts the Registry-proxied routes (GraphQL + REST)
// onto r. Split out from main() so this wiring can be exercised in
// routes_test.go without booting the Engine's full dependency graph (DB,
// NATS, gRPC) -- the tests only need a chi.Router, a Forwarder, and a Store.
func registerProxyRoutes(r chi.Router, proxy api.Forwarder, s store.Store, enforcers ...*enginemiddleware.RuntimeEnforcer) {
	registerProxyRoutesWithRuntimeContracts(r, proxy, s, nil, enforcers...)
}

func registerProxyRoutesWithRuntimeContracts(r chi.Router, proxy api.Forwarder, s store.Store, contractFetcher api.RuntimeContractFetcher, enforcers ...*enginemiddleware.RuntimeEnforcer) {
	enforcer := firstRuntimeEnforcer(enforcers)
	// Proxy routes authenticate locally, then RegistryProxy replaces inbound
	// auth with the Engine's licensed workspace identity.
	r.Post("/graphql", api.GraphQLProxyHandler(proxy, s, enforcer))

	restHandler := api.RESTProxyHandlerWithRuntimeContracts(proxy, s, contractFetcher, enforcer)
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

func firstRuntimeEnforcer(enforcers []*enginemiddleware.RuntimeEnforcer) *enginemiddleware.RuntimeEnforcer {
	if len(enforcers) == 0 {
		return nil
	}
	return enforcers[0]
}
