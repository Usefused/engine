package bridgehost

import (
	"context"
	"time"

	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
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

// startRevocations establishes LISTEN before the first session can become visible.
func (s *Server) startRevocations(ctx context.Context) error {
	listener, err := s.repo.SubscribeRevocations(ctx, s.instance)
	// Startup fails rather than silently falling back to heartbeat-only revocation.
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.revocationsReady = true
	s.mu.Unlock()
	go s.watchRevocations(ctx, listener)
	return nil
}

// watchRevocations fails closed on listener loss and reconnects before admitting new sessions.
func (s *Server) watchRevocations(ctx context.Context, listener *revocationListener) {
	for {
		pairingID, err := listener.Wait(ctx)
		// Notifications arrive only after the revoking transaction commits.
		if err == nil {
			s.cancelRevokedSession(pairingID)
			continue
		}
		s.stopRevocations()
		listener.Close()
		listener = s.reconnectRevocations(ctx)
		// Cancellation releases the dedicated connection and every active session.
		if listener == nil {
			return
		}
	}
}

// reconnectRevocations bounds retries while the transport remains closed to new installations.
func (s *Server) reconnectRevocations(ctx context.Context) *revocationListener {
	for {
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		listener, err := s.repo.SubscribeRevocations(ctx, s.instance)
		// A fresh subscription cannot lose previously admitted sessions: they were all cancelled.
		if err == nil {
			s.mu.Lock()
			s.revocationsReady = true
			s.mu.Unlock()
			return listener
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
