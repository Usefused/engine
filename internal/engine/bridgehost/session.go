package bridgehost

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Server owns bounded live sessions; all durable identities remain in PostgreSQL.
type Server struct {
	bridgev1.UnimplementedBridgeServiceServer
	repo             *Repository
	instance         string
	grpcURL          string
	discoveryReady   bool
	conn             *nats.Conn
	mu               sync.Mutex
	sessions         map[string]*session
	revocationsReady bool
}
type session struct {
	pairing Pairing
	out     chan *bridgev1.EngineFrame
	results map[string]chan *bridgev1.CallResult
	mu      sync.Mutex
	done    chan struct{}
	cancel  context.CancelCauseFunc
}

// New shares the Engine pool and internal broker while assigning a distinct replica identity.
func New(pool *pgxpool.Pool, conn *nats.Conn) *Server {
	return &Server{repo: &Repository{Pool: pool}, instance: uuid.NewString(), conn: conn, sessions: map[string]*session{}}
}

// Connect authenticates the installation and fences prior sessions before acknowledging readiness.
func (s *Server) Connect(stream grpc.BidiStreamingServer[bridgev1.BridgeFrame, bridgev1.EngineFrame]) error {
	p, err := s.authenticateHandshake(stream)
	// Only a bound installation can claim session ownership.
	if err != nil {
		return err
	}
	generation, err := s.repo.Claim(stream.Context(), p, s.instance)
	// A session without durable ownership cannot receive calls or publish events.
	if err != nil {
		return status.Error(codes.Unavailable, "bridge ownership unavailable")
	}
	p.Generation = generation
	ctx, cancel := context.WithCancelCause(stream.Context())
	defer cancel(nil)
	stream = &revocableStream{BidiStreamingServer: stream, ctx: ctx}
	current := &session{pairing: p, out: make(chan *bridgev1.EngineFrame, 64), results: map[string]chan *bridgev1.CallResult{}, done: make(chan struct{}), cancel: cancel}
	s.mu.Lock()
	// A disconnected notification listener cannot admit sessions that might miss revocation.
	if !s.revocationsReady {
		s.mu.Unlock()
		return status.Error(codes.Unavailable, "bridge revocation listener unavailable")
	}
	// Replacing a local generation also cancels its pending calls and blocked writer.
	if previous := s.sessions[p.ID]; previous != nil && previous.cancel != nil {
		previous.cancel(status.Error(codes.PermissionDenied, "bridge session replaced"))
	}
	s.sessions[p.ID] = current
	s.mu.Unlock()
	defer s.detach(current)
	// Recheck after registration so a revocation between Claim and registration cannot be missed.
	if err = s.repo.Renew(ctx, p.ID, generation); err != nil {
		return status.Error(codes.PermissionDenied, "bridge session revoked")
	}
	// Readiness is sent only after ownership is committed.
	if err = sendBounded(stream, &bridgev1.EngineFrame{Message: &bridgev1.EngineFrame_Welcome{Welcome: &bridgev1.Welcome{ProtocolMajor: 1, ProtocolMinor: 1, Generation: generation, DestinationId: p.DestinationID, MaxInflightEvents: 32, MaxPayloadBytes: MaxPayload}}}); err != nil {
		return err
	}
	return s.runSession(stream, current)
}

// detach removes only this generation so an old stream cannot evict its replacement.
func (s *Server) detach(current *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A reconnect may already have installed a newer session in this process.
	if s.sessions[current.pairing.ID] == current {
		delete(s.sessions, current.pairing.ID)
	}
	close(current.done)
}

// sessionRead carries either one bounded protocol frame or its terminal transport error.
type sessionRead struct {
	frame *bridgev1.BridgeFrame
	err   error
}

// receiveFrames prevents a slow session handler from building an unbounded input queue.
func receiveFrames(stream grpc.BidiStreamingServer[bridgev1.BridgeFrame, bridgev1.EngineFrame], stop <-chan struct{}) <-chan sessionRead {
	incoming := make(chan sessionRead, 1)
	// gRPC releases Recv when the parent handler returns; stop releases a blocked channel sender.
	go func() {
		for {
			frame, err := stream.Recv()
			select {
			case incoming <- sessionRead{frame, err}:
			case <-stop:
				return
			}
			// A terminal read must not hot-loop on a closed transport.
			if err != nil {
				return
			}
		}
	}()
	return incoming
}

// runSession owns one serialized writer and renews idle sessions without retaining an input backlog.
func (s *Server) runSession(stream grpc.BidiStreamingServer[bridgev1.BridgeFrame, bridgev1.EngineFrame], current *session) error {
	stop := make(chan struct{})
	defer close(stop)
	incoming := receiveFrames(stream, stop)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		var err error
		// Every selected event either advances the session or returns a terminal authorization/transport failure.
		select {
		case <-stream.Context().Done():
			return context.Cause(stream.Context())
		case <-ticker.C:
			err = s.sendAuthorized(stream, current, &bridgev1.EngineFrame{Message: &bridgev1.EngineFrame_Heartbeat{Heartbeat: &bridgev1.Heartbeat{Generation: current.pairing.Generation}}})
		case frame := <-current.out:
			err = s.sendAuthorized(stream, current, frame)
		case item := <-incoming:
			err = s.handleReceived(stream, current, item)
		}
		// EOF leaves unacknowledged events available for replay without claiming failed commands never ran.
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// sendAuthorized rechecks ownership immediately before any live dispatch or heartbeat.
func (s *Server) sendAuthorized(stream grpc.BidiStreamingServer[bridgev1.BridgeFrame, bridgev1.EngineFrame], current *session, frame *bridgev1.EngineFrame) error {
	// A replaced generation cannot receive new work even if its local session remains open.
	if err := s.repo.Renew(stream.Context(), current.pairing.ID, current.pairing.Generation); err != nil {
		return status.Error(codes.PermissionDenied, "bridge session revoked")
	}
	return sendBounded(stream, frame)
}

// sendBounded releases the handler when a peer stops reading, which also cancels gRPC's blocked writer.
func sendBounded(stream grpc.BidiStreamingServer[bridgev1.BridgeFrame, bridgev1.EngineFrame], frame *bridgev1.EngineFrame) error {
	done := make(chan error, 1)
	// The buffered result cannot strand the sender after the timeout returns the handler.
	go func() { done <- stream.Send(frame) }()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-stream.Context().Done():
		return context.Cause(stream.Context())
	case <-timer.C:
		return status.Error(codes.DeadlineExceeded, "bridge peer is not reading")
	}
}

// handleReceived distinguishes transport termination from protocol frames that need durable acknowledgement.
func (s *Server) handleReceived(stream grpc.BidiStreamingServer[bridgev1.BridgeFrame, bridgev1.EngineFrame], current *session, item sessionRead) error {
	// Transport failure carries no valid protocol frame.
	if item.err != nil {
		return item.err
	}
	reply, err := s.handleFrame(stream.Context(), current, item.frame)
	// Results and heartbeats are one-way and require no reply.
	if err != nil || reply == nil {
		return err
	}
	return sendBounded(stream, reply)
}

// handleFrame translates durable decisions without exposing database or provider diagnostics.
func (s *Server) handleFrame(ctx context.Context, current *session, frame *bridgev1.BridgeFrame) (*bridgev1.EngineFrame, error) {
	// Every frame revalidates the generation before it can settle or create work.
	if err := s.repo.Renew(ctx, current.pairing.ID, current.pairing.Generation); err != nil {
		return nil, status.Error(codes.PermissionDenied, "bridge session revoked")
	}
	// Inventory is acknowledged only after the authoritative removal set commits.
	if message := frame.GetInventory(); message != nil {
		return s.acceptInventory(ctx, current, message), nil
	}
	// Existing frame kinds retain their single durable acceptance paths.
	switch message := frame.Message.(type) {
	case *bridgev1.BridgeFrame_Manifest:
		err := s.repo.PutManifest(ctx, current.pairing, message.Manifest)
		if err != nil {
			return rejection(err, message.Manifest.Hash), nil
		}
		return &bridgev1.EngineFrame{Message: &bridgev1.EngineFrame_ManifestAccepted{ManifestAccepted: &bridgev1.ManifestAccepted{ProfileId: message.Manifest.ProfileId, Revision: message.Manifest.Revision, Hash: message.Manifest.Hash}}}, nil
	case *bridgev1.BridgeFrame_Event:
		err := s.repo.AcceptEvent(ctx, current.pairing, message.Event)
		if err != nil {
			return rejection(err, message.Event.EventId), nil
		}
		return &bridgev1.EngineFrame{Message: &bridgev1.EngineFrame_EventAccepted{EventAccepted: &bridgev1.EventAccepted{EventId: message.Event.EventId, DestinationId: current.pairing.DestinationID}}}, nil
	case *bridgev1.BridgeFrame_Result:
		return nil, acceptCallResult(current, message.Result)
	case *bridgev1.BridgeFrame_Heartbeat:
		if message.Heartbeat.Generation != current.pairing.Generation {
			return nil, status.Error(codes.InvalidArgument, "generation mismatch")
		}
		return nil, nil
	default:
		// A second hello or unknown frame cannot mutate an established session.
		return nil, status.Error(codes.InvalidArgument, "unexpected bridge frame")
	}
}

// rejection keeps retryability explicit without promising that a command can be safely replayed.
func rejection(err error, reference string) *bridgev1.EngineFrame {
	code, retry := "storage_unavailable", true
	switch {
	case errors.Is(err, ErrContract):
		code, retry = "contract_invalid", false
	case errors.Is(err, ErrConflict):
		code, retry = "identity_conflict", false
	case errors.Is(err, ErrDenied):
		code, retry = "permission_denied", false
	case errors.Is(err, ErrCapacity):
		code = "capacity_exhausted"
	}
	return &bridgev1.EngineFrame{Message: &bridgev1.EngineFrame_Error{Error: &bridgev1.ProtocolError{Code: code, RefId: reference, Retryable: retry}}}
}

// authenticateHandshake binds one credential to one protocol-major installation identity.
func (s *Server) authenticateHandshake(stream grpc.BidiStreamingServer[bridgev1.BridgeFrame, bridgev1.EngineFrame]) (Pairing, error) {
	md, _ := metadata.FromIncomingContext(stream.Context())
	values := md.Get("authorization")
	// Ambiguous credentials are rejected rather than choosing one header.
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return Pairing{}, status.Error(codes.Unauthenticated, "bridge credential required")
	}
	p, err := s.repo.Authenticate(stream.Context(), strings.TrimPrefix(values[0], "Bearer "))
	if err != nil {
		return Pairing{}, status.Error(codes.Unauthenticated, "bridge credential rejected")
	}
	first, err := stream.Recv()
	// The handshake must bind the credential to its persisted installation and pairing.
	if err != nil || first.GetHello() == nil {
		return Pairing{}, status.Error(codes.InvalidArgument, "hello required")
	}
	hello := first.GetHello()
	if hello.ProtocolMajor != 1 || hello.ProtocolMinor < 1 || hello.PairingId != p.ID || hello.InstallationId != p.InstallationID {
		return Pairing{}, status.Error(codes.FailedPrecondition, "bridge protocol or identity mismatch")
	}
	p.DurableCalls = hello.DurableCalls
	return p, nil
}

// acceptCallResult matches a result only to the current generation's outstanding request.
func acceptCallResult(current *session, result *bridgev1.CallResult) error {
	// Late results from superseded sessions cannot resolve a current invocation.
	if result.Generation != current.pairing.Generation {
		return status.Error(codes.InvalidArgument, "generation mismatch")
	}
	current.mu.Lock()
	destination := current.results[result.RequestId]
	current.mu.Unlock()
	if destination != nil {
		select {
		case destination <- result:
		default:
		}
	}
	return nil
}

// acceptInventory sends a receipt only after durable workspace reconciliation succeeds.
func (s *Server) acceptInventory(ctx context.Context, current *session, message *bridgev1.Inventory) *bridgev1.EngineFrame {
	// Rejected snapshots cannot advance the Bridge synchronization barrier.
	if err := s.repo.PutInventory(ctx, current.pairing, message); err != nil {
		return rejection(err, message.Hash)
	}
	return &bridgev1.EngineFrame{Message: &bridgev1.EngineFrame_InventoryAccepted{InventoryAccepted: &bridgev1.InventoryAccepted{Hash: message.Hash}}}
}
