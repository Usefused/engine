package bridgehost

import (
	"context"
	"github.com/google/uuid"
)

type snapshotCallKey struct{}
type snapshotCall struct {
	Key string
	Run uuid.UUID
}

// WithSnapshotCall carries only an Engine-admitted logical call identity, never client routing overrides.
func WithSnapshotCall(ctx context.Context, key string, run uuid.UUID) context.Context {
	return context.WithValue(ctx, snapshotCallKey{}, snapshotCall{key, run})
}
func SnapshotCall(ctx context.Context) (string, uuid.UUID) {
	v, _ := ctx.Value(snapshotCallKey{}).(snapshotCall)
	return v.Key, v.Run
}
func CallIdentity(ctx context.Context) string {
	key, run := SnapshotCall(ctx)
	if run == uuid.Nil {
		return ""
	}
	return key
}

// AuthorizeSnapshot rechecks consent, current ownership and contracts without requiring a live connection.
func (s *Server) AuthorizeSnapshot(ctx context.Context, account string, pin Pin, input []byte) error {
	_, _, _, err := s.resolveAdmittedPin(ctx, account, pin, input, false)
	return err
}
