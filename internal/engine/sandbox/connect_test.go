package sandbox

import (
	"context"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// recordingCache records handshake calls so the gRPC Connect/Disconnect handlers
// can be asserted in isolation.
type recordingCache struct {
	connectedID    string
	disconnectedID string
	connectErr     error
}

func (r *recordingCache) ConnectSDK(ctx context.Context, artifactID string) error {
	if r.connectErr != nil {
		return r.connectErr
	}
	r.connectedID = artifactID
	return nil
}
func (r *recordingCache) DisconnectSDK(artifactID string) { r.disconnectedID = artifactID }
func (r *recordingCache) GetOrFetchServiceMetadata(ctx context.Context, artifactID string, serviceID string) (*fusedobject.ServiceMetadata, error) {
	return nil, nil
}
func (r *recordingCache) GetEndpoint(ctx context.Context, artifactID string, serviceID string, endpointName string) (*fusedobject.Endpoint, error) {
	return nil, nil
}
func (r *recordingCache) ListEndpointsForSelection(ctx context.Context, artifactID string, sel models.SDKSelection) ([]fusedobject.Endpoint, error) {
	return nil, nil
}

func (r *recordingCache) GetArtifactScope(ctx context.Context, artifactID string) (string, []byte, error) {
	return "", nil, nil
}
func (r *recordingCache) Invalidate(serviceID string) {}
