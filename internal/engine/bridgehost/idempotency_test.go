package bridgehost

import (
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSnapshotCallRejectsOldBridgeBeforeDispatch(t *testing.T) {
	p := Pairing{ID: "bridge", Generation: 1}
	current := &session{pairing: p, out: make(chan *bridgev1.EngineFrame, 1), results: map[string]chan *bridgev1.CallResult{}, done: make(chan struct{})}
	s := &Server{sessions: map[string]*session{"bridge": current}}
	result := s.invokeLocal(t.Context(), p, &bridgev1.Call{RequestId: "request", Generation: 1, IdempotencyKey: "stable"})
	require.Equal(t, "not_started", result.Outcome)
	require.Equal(t, "idempotency_unsupported", result.ErrorCode)
	require.Empty(t, current.out)
	require.Empty(t, current.results)
}
