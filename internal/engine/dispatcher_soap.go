package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	xj "github.com/basgys/goxml2json"
	"github.com/tiaguinho/gosoap"
)

// executeSOAP handles SOAP method calls by dynamically creating a gosoap client
// using the RawWSDL stored in the service object, forwarding the request,
// and converting the XML response back to JSON.
func (d *Dispatcher) executeSOAP(
	ctx context.Context,
	srv *models.Service,
	obj *models.IntegrationObject,
	params map[string]any,
	credentials map[string]any,
	bucketValues []store.BucketValue,
	stream ResponseStream,
) (int, error) {
	if srv.RawWSDL == "" {
		return 500, fmt.Errorf("missing RawWSDL on service to execute SOAP request")
	}

	tmpfile, err := os.CreateTemp("", "wsdl-*.xml")
	if err != nil {
		return 500, fmt.Errorf("failed to create temp wsdl file: %w", err)
	}
	defer os.Remove(tmpfile.Name())
	tmpfile.Write([]byte(srv.RawWSDL))
	tmpfile.Close()

	client, err := gosoap.SoapClientWithConfig(tmpfile.Name(), d.client, &gosoap.Config{
		Dump: true, // Allows us to see the request/response
	})
	if err != nil {
		return 500, fmt.Errorf("failed to initialize SOAP client: %w", err)
	}

	p := gosoap.Params{}
	for k, v := range params {
		p[k] = v
	}

	// The SOAP action name is typically the NormalizedPath.
	actionName := obj.NormalizedPath
	res, err := client.Call(actionName, p)

	// Check if network request itself failed
	if err != nil && res == nil {
		return 502, fmt.Errorf("SOAP network request failed: %w", err)
	}

	// Convert the raw XML response (res.Body) to JSON
	xmlReader := bytes.NewReader(res.Body)
	jsonRes, err := xj.Convert(xmlReader)
	if err != nil {
		return 502, fmt.Errorf("failed to convert SOAP XML response to JSON: %w", err)
	}

	jsonBytes := jsonRes.Bytes()

	// Stream the JSON response
	if sendErr := stream.Send(jsonBytes); sendErr != nil {
		return 200, fmt.Errorf("failed to stream JSON response: %w", sendErr)
	}

	return 200, nil
}
