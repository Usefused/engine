package sandbox

import "github.com/google/uuid"

// TerminateMCPSessionsForApp closes every local transport session for one
// immutable app version. Cluster-wide token revocation uses the separate,
// versioned invalidation fan-out because it has a different identity boundary.
func TerminateMCPSessionsForApp(appID string) int {
	return terminateMatchingMCPSessions("app_deactivated", func(sess *mcpSession) bool {
		return sess.appID == appID
	})
}

// MCPSessionTokenInvalidator joins the token-invalidation fan-out without
// changing its wire contract or exposing bearer material to subscribers.
type MCPSessionTokenInvalidator struct{}

func (MCPSessionTokenInvalidator) InvalidateToken(tokenID uuid.UUID) int {
	return terminateMatchingMCPSessions("token_revoked", func(sess *mcpSession) bool {
		return sess.tokenID == tokenID
	})
}

func terminateMatchingMCPSessions(reason string, matches func(*mcpSession) bool) int {
	var sessionIDs []string
	mcpSessions.RLock()
	for sessionID, sess := range mcpSessions.m {
		if matches(sess) {
			sessionIDs = append(sessionIDs, sessionID)
		}
	}
	mcpSessions.RUnlock()
	// Termination takes the write lock, so collect under the read lock and act
	// afterwards rather than upgrading a lock while iterating shared state.
	for _, sessionID := range sessionIDs {
		terminateMCPSession(sessionID, reason)
	}
	return len(sessionIDs)
}
