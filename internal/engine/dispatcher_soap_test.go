package engine

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

func TestExecuteSOAPMissingWSDL(t *testing.T) {
	dispatcher := NewDispatcher()
	srv := &models.Service{}
	obj := &models.IntegrationObject{
		Method: "SOAP",
	}

	stream := NewBufferStream()
	status, err := dispatcher.executeSOAP(context.Background(), srv, obj, nil, nil, nil, stream)

	if status != 500 {
		t.Errorf("expected 500 status, got %d", status)
	}

	if err == nil || err.Error() != "missing RawWSDL on service to execute SOAP request" {
		t.Errorf("expected missing RawWSDL error, got %v", err)
	}
}
