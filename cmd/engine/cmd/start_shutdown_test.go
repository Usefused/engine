package cmd

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

// TestShutdownGRPCServerClosesPersistentStream reproduces the open-stream restart hang with a real RPC.
func TestShutdownGRPCServerClosesPersistentStream(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	defer listener.Close()
	server := grpc.NewServer()
	defer server.Stop()
	healthpb.RegisterHealthServer(server, health.NewServer())
	// Serve a real streaming RPC without depending on host ports or a database.
	go func() { _ = server.Serve(listener) }()
	// The in-memory dialer keeps this regression isolated from the running Engine.
	dial := func(context.Context, string) (net.Conn, error) { return listener.Dial() }
	conn, err := grpc.NewClient("passthrough:///shutdown-test", grpc.WithContextDialer(dial), grpc.WithTransportCredentials(insecure.NewCredentials()))
	// A failed setup must not masquerade as successful shutdown.
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
	// Establish the stream before starting graceful shutdown.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer drainCancel()
	done := make(chan struct{})
	// Detect an unbounded GracefulStop without hanging the entire test suite.
	go func() { shutdownGRPCServer(drainCtx, server); close(done) }()
	// The active stream must be terminated when the drain deadline is exhausted.
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("shutdown did not respect the drain deadline")
	}
	// The client must observe closure rather than retaining a stranded live stream.
	if _, err := stream.Recv(); err == nil {
		t.Fatal("stream survived server shutdown")
	}
}

// TestShutdownGRPCServerDrainsIdleServer ensures normal shutdown does not wait for the deadline.
func TestShutdownGRPCServerDrainsIdleServer(t *testing.T) {
	server := grpc.NewServer()
	defer server.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownGRPCServer(ctx, server)
	// An idle server should finish while its grace budget remains available.
	if ctx.Err() != nil {
		t.Fatal("idle shutdown exhausted its drain budget")
	}
}
