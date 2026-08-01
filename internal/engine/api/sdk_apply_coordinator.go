package api

import (
	"sync"

	"github.com/google/uuid"
)

type sdkArtifactApplyLock struct {
	mutex sync.Mutex
	refs  int
}

type sdkArtifactApplyCoordinator struct {
	mutex   sync.Mutex
	entries map[uuid.UUID]*sdkArtifactApplyLock
}

func newSDKArtifactApplyCoordinator() *sdkArtifactApplyCoordinator {
	return &sdkArtifactApplyCoordinator{entries: make(map[uuid.UUID]*sdkArtifactApplyLock)}
}

func (c *sdkArtifactApplyCoordinator) lock(artifactID uuid.UUID) func() {
	c.mutex.Lock()
	entry := c.entries[artifactID]
	if entry == nil {
		entry = &sdkArtifactApplyLock{}
		c.entries[artifactID] = entry
	}
	entry.refs++
	c.mutex.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		c.mutex.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(c.entries, artifactID)
		}
		c.mutex.Unlock()
	}
}

var sdkGenerationApplies = newSDKArtifactApplyCoordinator()
