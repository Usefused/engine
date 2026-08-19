package api

import (
	"maps"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
)

// TestConnectSessionResourceInputUsesHostedDecoder locks the shared canonical
// string-map behavior while retaining the callback's stable public error.
func TestConnectSessionResourceInputUsesHostedDecoder(t *testing.T) {
	raw := []byte(`{"site":"acme","region":"eu"}`)
	hosted, hostedErr := decodeConnectInputValues(raw)
	callback, callbackErr := connectSessionResourceInput(&store.ConnectSession{ResourceInputJSON: raw})
	// Both paths must expose the same decoded map from the one production helper.
	if hostedErr != nil || callbackErr != nil || !maps.Equal(hosted, callback) {
		t.Fatalf("hosted/callback resource input = %#v/%#v errors=%v/%v", hosted, callback, hostedErr, callbackErr)
	}
	_, callbackErr = connectSessionResourceInput(&store.ConnectSession{ResourceInputJSON: []byte(`{"site":1}`)})
	// Stored type corruption remains a bounded callback error without raw JSON details.
	if callbackErr == nil || callbackErr.Error() != "resource input is invalid" {
		t.Fatalf("invalid callback resource input error = %v", callbackErr)
	}
}
