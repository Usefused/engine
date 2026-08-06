package api

import (
	"sync"

	"github.com/google/uuid"
)

type sdkAppApplyLock struct {
	mutex sync.Mutex
	refs  int
}

type sdkAppApplyCoordinator struct {
	mutex   sync.Mutex
	entries map[uuid.UUID]*sdkAppApplyLock
}

func newSDKAppApplyCoordinator() *sdkAppApplyCoordinator {
	return &sdkAppApplyCoordinator{entries: make(map[uuid.UUID]*sdkAppApplyLock)}
}

func (c *sdkAppApplyCoordinator) lock(appID uuid.UUID) func() {
	c.mutex.Lock()
	entry := c.entries[appID]
	if entry == nil {
		entry = &sdkAppApplyLock{}
		c.entries[appID] = entry
	}
	entry.refs++
	c.mutex.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		c.mutex.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(c.entries, appID)
		}
		c.mutex.Unlock()
	}
}

var sdkGenerationApplies = newSDKAppApplyCoordinator()
