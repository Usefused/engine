package connectauth

import (
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/shared/oauthmapping"
)

// decodeSelectedTokenResponse preserves outer ownership claims while selecting exactly one credential principal.
func decodeSelectedTokenResponse(contentType string, body []byte, path []string) (TokenResponse, error) {
	// Contracts without mapping retain standard JSON and form compatibility.
	if len(path) == 0 {
		return decodeTokenResponse(contentType, body)
	}
	// Validate locally as well as at import so malformed stored metadata fails closed.
	if err := oauthmapping.Validate("", path); err != nil {
		return TokenResponse{}, err
	}
	selected := json.RawMessage(body)
	for _, key := range path {
		var object map[string]json.RawMessage
		// Missing/non-object principals must never fall back to a broader top-level bot credential.
		if json.Unmarshal(selected, &object) != nil || len(object[key]) == 0 {
			return TokenResponse{}, errors.New("selected OAuth token response object is missing")
		}
		selected = object[key]
	}
	var token TokenResponse
	// Arrays, scalars, null, and credential-free objects cannot form a usable connection.
	if json.Unmarshal(selected, &token) != nil || token.AccessToken == "" {
		return TokenResponse{}, errors.New("selected OAuth response omitted access_token")
	}
	token.RawResponse = append(json.RawMessage(nil), body...)
	return token, nil
}
