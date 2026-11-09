package engine

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

func TestExecuteRejectsSOAPOperations(t *testing.T) {
	dispatcher := &Dispatcher{}
	srv := &models.Service{}
	for _, method := range []string{"SOAP", "soap"} {
		obj := &models.IntegrationObject{Method: method, NormalizedPath: "GetInvoice"}
		status, err := dispatcher.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, NewBufferStream())
		if status != http.StatusNotImplemented || !errors.Is(err, ErrSOAPUnsupported) {
			t.Fatalf("method %q: got status %d err %v, want 501 ErrSOAPUnsupported", method, status, err)
		}
	}
}
