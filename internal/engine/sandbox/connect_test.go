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

func (r *recordingCache) ConnectSDK(ctx context.Context, appID string) error {
	if r.connectErr != nil {
		return r.connectErr
	}
	r.connectedID = appID
	return nil
}
func (r *recordingCache) DisconnectSDK(appID string) { r.disconnectedID = appID }
func (r *recordingCache) GetOrFetchServiceMetadata(ctx context.Context, appID string, serviceID string) (*fusedobject.ServiceMetadata, error) {
	return nil, nil
}
func (r *recordingCache) GetEndpoint(ctx context.Context, appID string, serviceID string, endpointName string) (*fusedobject.Endpoint, error) {
	return nil, nil
}
func (r *recordingCache) ListEndpointsForSelection(ctx context.Context, appID string, sel models.SDKSelection) ([]fusedobject.Endpoint, error) {
	return nil, nil
}

func (r *recordingCache) GetAppRuntime(ctx context.Context, appID string) (string, []byte, error) {
	return "", nil, nil
}
func (r *recordingCache) Invalidate(serviceID string) {}
