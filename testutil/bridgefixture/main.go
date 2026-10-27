// Command bridgefixture runs an explicitly local disposable integration target for the Bridge client.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
)

// main requires an explicit test database and writes ephemeral credentials only to a private file.
func main() {
	dsn := flag.String("dsn", "", "Dedicated test PostgreSQL URL")
	output := flag.String("output", "", "Private fixture metadata output path")
	profile := flag.String("profile", "fixture", "Selected Bridge profile ID")
	flag.Parse()
	// Never discover production credentials or silently select a live database.
	if *dsn == "" || *output == "" {
		panic("--dsn and --output are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Errors intentionally omit database credentials and fixture tokens.
	if err := run(ctx, *dsn, *output, *profile); err != nil {
		fmt.Fprintln(os.Stderr, "Bridge fixture failed:", err)
		os.Exit(1)
	}
}

// run initializes an isolated schema and serves only loopback transports until cancellation.
func run(ctx context.Context, dsn, output, profile string) error {
	pool, cleanup, err := openFixtureDatabase(ctx, dsn)
	// The local fixture cannot start without its own fully initialized schema.
	if err != nil {
		return err
	}
	defer cleanup()
	repository := store.NewPostgresStore(pool)
	actor, control, err := fixtureActor(ctx, repository)
	// Pairing and app publication use the same explicit test workspace authority.
	if err != nil {
		return err
	}
	p, secret, err := (&bridgehost.Repository{Pool: pool}).Create(ctx, actor, uuid.NewString(), "Integration fixture", "workspace")
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		listener.Close()
		return err
	}
	host := bridgehost.New(pool, nil)
	rpc := grpc.NewServer()
	bridgev1.RegisterBridgeServiceServer(rpc, host)
	go rpc.Serve(listener)
	defer rpc.Stop()
	router := chi.NewRouter()
	// Fixture-only control identity still requires a random credential; all real pairing checks remain active.
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "Bearer "+control {
				r = r.WithContext(accesscontrol.ContextWithActor(r.Context(), actor))
			}
			next.ServeHTTP(w, r)
		})
	})
	host.MountHTTP(router, "http://"+httpListener.Addr().String(), "http://"+listener.Addr().String())
	// Invoke publishes a selected test app and uses its real token-authenticated execution route.
	router.Post("/fixture/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+control {
			w.WriteHeader(401)
			return
		}
		var input struct {
			Pin   bridgehost.Pin  `json:"pin"`
			Input json.RawMessage `json:"input"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&input) != nil {
			w.WriteHeader(400)
			return
		}
		raw, err := invokeConsumerFixture(r.Context(), pool, repository, host, actor, input.Pin, input.Input)
		if err != nil {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	httpServer := &http.Server{Handler: router}
	go httpServer.Serve(httpListener)
	defer httpServer.Close()
	metadata := map[string]any{"pairing_id": p.ID, "destination_id": p.DestinationID, "installation_id": p.InstallationID, "credential": secret, "control_credential": control, "grpc_url": "http://" + listener.Addr().String(), "http_url": "http://" + httpListener.Addr().String(), "profile_id": profile, "scope": "workspace", "database_schema": pool.Config().ConnConfig.RuntimeParams["search_path"]}
	return serveFixtureUntilCancelled(ctx, output, metadata)
}

// serveFixtureUntilCancelled exposes credentials only in a private file while transports are alive.
func serveFixtureUntilCancelled(ctx context.Context, output string, metadata map[string]any) error {
	raw, err := json.Marshal(metadata)
	// Invalid metadata cannot leave a partially usable fixture advertised.
	if err != nil {
		return err
	}
	// Fixture credentials must never appear in stdout or ordinary repository files.
	if err = os.WriteFile(output, raw, 0600); err != nil {
		return err
	}
	fmt.Println("Bridge fixture ready; private metadata written")
	<-ctx.Done()
	return nil
}

// openFixtureDatabase confines all fixture writes to a disposable schema and exposes one cleanup owner.
func openFixtureDatabase(ctx context.Context, dsn string) (*pgxpool.Pool, func(), error) {
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("test database unavailable")
	}

	complete := false
	// Failed initialization must release its administration connection too.
	defer func() {
		if !complete {
			admin.Close()
		}
	}()
	schema := "bridge_fixture_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return nil, nil, err
	}

	// A schema created before a later failure remains owned by this fixture initialization.
	defer func() {
		if !complete {
			_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	}()
	parsed, err := url.Parse(dsn)
	if err != nil {
		return nil, nil, err
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	pool, err := db.InitEnginePostgres(ctx, parsed.String())
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	}
	complete = true
	return pool, cleanup, nil
}

// fixtureActor bootstraps a real owner and returns only the bounded permissions used by integration controls.
func fixtureActor(ctx context.Context, repository store.Store) (accesscontrol.Actor, string, error) {
	account := uuid.New()
	workspace, err := repository.BootstrapWorkspace(ctx, account, "Bridge integration fixture")
	if err != nil {
		return accesscontrol.Actor{}, "", err
	}
	control := "fixture_" + uuid.NewString()
	owner, err := accesscontrol.BootstrapOwner(ctx, repository.(accesscontrol.BootstrapRepository), account, control)
	if err != nil {
		return accesscontrol.Actor{}, "", err
	}
	authz, err := accesscontrol.NewAuthorizationSnapshot(owner.Revision, accesscontrol.Grant{Permission: accesscontrol.PermissionBridgeManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}}, accesscontrol.Grant{Permission: accesscontrol.PermissionBridgeRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}})
	if err != nil {
		return accesscontrol.Actor{}, "", err
	}
	actor := accesscontrol.Actor{AccountID: account, WorkspaceID: workspace, SubjectID: owner.SubjectID, Authorization: authz}
	return actor, control, nil
}
