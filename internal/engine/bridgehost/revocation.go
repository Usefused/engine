package bridgehost

import (
	"context"
	"time"

	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// revocableStream lets committed revocation interrupt database work and a blocked writer.
type revocableStream struct {
	grpc.BidiStreamingServer[bridgev1.BridgeFrame, bridgev1.EngineFrame]
	ctx context.Context
}

// Context preserves the transport lifetime while adding server-side authorization cancellation.
func (s *revocableStream) Context() context.Context { return s.ctx }

// listenRevocations dedicates one connection per replica without consuming the request pool.
func (s *Server) listenRevocations(ctx context.Context) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	config := s.repo.Pool.Config().ConnConfig.Copy()
	config.RuntimeParams["application_name"] = "bridge-revocation-" + s.instance
	conn, err := pgx.ConnectConfig(ctx, config)
	// A failed subscription must never admit a session without an invalidation path.
	if err != nil {
		return nil, err
	}
	var channel string
	err = conn.QueryRow(ctx, `SELECT 'fused_bridge_revoked_' || md5(current_schema())`).Scan(&channel)
	// Channel names are schema-specific so isolated workspaces cannot invalidate one another.
	if err == nil {
		_, err = conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize())
	}
	// Do not leak a dedicated connection when LISTEN cannot be established.
	if err != nil {
		closeRevocationConnection(conn)
		return nil, err
	}
	return conn, nil
}

// closeRevocationConnection bounds shutdown even if PostgreSQL has disappeared.
func closeRevocationConnection(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

// startRevocations establishes LISTEN before the first session can become visible.
func (s *Server) startRevocations(ctx context.Context) error {
	conn, err := s.listenRevocations(ctx)
	// Startup fails rather than silently falling back to heartbeat-only revocation.
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.revocationsReady = true
	s.mu.Unlock()
	go s.watchRevocations(ctx, conn)
	return nil
}

// watchRevocations fails closed on listener loss and reconnects before admitting new sessions.
func (s *Server) watchRevocations(ctx context.Context, conn *pgx.Conn) {
	for {
		notification, err := conn.WaitForNotification(ctx)
		// Notifications arrive only after the revoking transaction commits.
		if err == nil {
			s.cancelRevokedSession(notification.Payload)
			continue
		}
		s.stopRevocations()
		closeRevocationConnection(conn)
		conn = s.reconnectRevocations(ctx)
		// Cancellation releases the dedicated connection and every active session.
		if conn == nil {
			return
		}
	}
}

// reconnectRevocations bounds retries while the transport remains closed to new installations.
func (s *Server) reconnectRevocations(ctx context.Context) *pgx.Conn {
	for {
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		conn, err := s.listenRevocations(ctx)
		// A fresh subscription cannot lose previously admitted sessions: they were all cancelled.
		if err == nil {
			s.mu.Lock()
			s.revocationsReady = true
			s.mu.Unlock()
			return conn
		}
	}
}

// cancelRevokedSession wakes idle reads and pending calls on the replica owning this pairing.
func (s *Server) cancelRevokedSession(pairingID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.sessions[pairingID]
	// Other replicas and already detached sessions legitimately have no local entry.
	if current != nil && current.cancel != nil {
		current.cancel(status.Error(codes.PermissionDenied, "bridge consent or pairing revoked"))
	}
}

// stopRevocations closes all sessions because notifications during listener loss cannot be recovered.
func (s *Server) stopRevocations() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revocationsReady = false
	for _, current := range s.sessions {
		// Only actual gRPC sessions own an authorization cancellation function.
		if current.cancel != nil {
			current.cancel(status.Error(codes.Unavailable, "bridge revocation listener unavailable"))
		}
	}
}
