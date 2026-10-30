package sandbox

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
)

// mcpReplacementSessionResponse follows MCP's 404 reinitialization contract after a valid token refresh.
func mcpReplacementSessionResponse(ctx context.Context, sess *mcpSession, token string) (int, error) {
	// Only a live replacement from the original grant may retire the child and its old expiry context.
	if rotatedMCPSessionCredential(ctx, sess, token) {
		terminateMCPSession(sess.sessionID, "token_expired")
		return http.StatusNotFound, errMCPSessionUnavailable
	}
	return http.StatusUnauthorized, errors.New("invalid Authorization header")
}

// rotatedMCPSessionCredential recognizes only a live replacement of the session's original OAuth grant.
func rotatedMCPSessionCredential(ctx context.Context, sess *mcpSession, token string) bool {
	// Manual execution tokens never gain cross-token session authority.
	if sess.oauthGrantID == uuid.Nil {
		return false
	}
	identity, err := validateMCPToken(ctx, sess.appID, token)
	return err == nil && identity.OAuthGrantID == sess.oauthGrantID && identity.TokenID != sess.tokenID
}
