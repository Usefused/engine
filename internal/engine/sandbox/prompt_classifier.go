package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ClassifyServiceOperation grounds prompt selection in a complete, visible Registry version before sharing bounded discovery prose with Jev.
func (c *HTTPRegistryClient) ClassifyServiceOperation(ctx context.Context, serviceID uuid.UUID, version, intent string) (string, error) {
	// Unbounded or unpinned input cannot initiate catalogue reads or paid inference.
	if serviceID == uuid.Nil || strings.TrimSpace(version) == "" || len(version) > 256 || strings.TrimSpace(intent) == "" || len(intent) > 4096 {
		return "", errors.New("operation discovery requires a service, exact version, and 1–4096 byte intent")
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	operations, err := c.promptOperationCatalogue(ctx, serviceID, version)
	// A missing or incomplete catalogue cannot safely produce an operation proposal.
	if err != nil {
		return "", err
	}
	names := make(map[string]bool, len(operations))
	for _, op := range operations {
		// Duplicate or malformed names would make opaque classifier labels ambiguous.
		if strings.TrimSpace(op.OperationName) == "" || len(op.OperationName) > 1024 || names[op.OperationName] {
			return "", errors.New("operation discovery catalogue is invalid")
		}
		names[op.OperationName] = true
	}
	// Explicit IDs already provide an exact grounding and require no external inference.
	if names[strings.TrimSpace(intent)] {
		return strings.TrimSpace(intent), nil
	}
	// Empty versions have no supported capability and need no paid call.
	if len(operations) == 0 {
		return "", nil
	}
	// Never classify a truncated subset of a large service.
	if len(operations) > 2048 {
		return "", errors.New("Jev operation discovery supports at most 2048 operations; use an exact operation ID")
	}
	selected, err := c.ClassifyOperations(ctx, intent, operations)
	// Provider failures remain explicit; text search is not a substitute for the selected classifier.
	if err != nil {
		return "", errors.New("Jev operation discovery is unavailable; retry or use an exact operation ID")
	}
	// No-match is valid, but provider output may never introduce a name outside this immutable catalogue.
	if selected != "" && !names[selected] {
		return "", errors.New("classifier returned an unauthorized operation")
	}
	return selected, nil
}

// promptOperationCatalogue requests only discovery fields; Registry enforces visibility for the licensed account and exact service version.
func (c *HTTPRegistryClient) promptOperationCatalogue(ctx context.Context, serviceID uuid.UUID, version string) ([]ClassifierOperation, error) {
	request, err := c.newGraphQLRequest(ctx, graphqlQuery{
		Query:     `query PromptOperationCatalogue($serviceId: String!, $version: String!) { serviceOperations(serviceId: $serviceId, version: $version) { name description } }`,
		Variables: map[string]interface{}{"serviceId": serviceID.String(), "version": version},
	})
	// Construction and transport errors must not expose the licensed Registry URL or headers.
	if err != nil {
		return nil, errors.New("operation catalogue unavailable")
	}
	response, err := c.doWithCallerDeadline(request)
	if err != nil {
		return nil, errors.New("operation catalogue unavailable")
	} // Keep credential-bearing transport diagnostics private.
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("operation catalogue unavailable")
	} // Visibility failures cannot fall back to another version.
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return nil, errors.New("operation catalogue exceeds the response limit")
	} // Bound raw descriptions before decoding.
	var result struct {
		Data struct {
			Operations []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"serviceOperations"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	// GraphQL errors and absent data are failures, not an authoritative empty catalogue.
	if json.Unmarshal(body, &result) != nil || len(result.Errors) != 0 || result.Data.Operations == nil {
		return nil, errors.New("operation catalogue unavailable")
	}
	operations := make([]ClassifierOperation, 0, len(result.Data.Operations))
	for _, op := range result.Data.Operations {
		operations = append(operations, ClassifierOperation{OperationName: op.Name, Description: classifierDescription(op.Description)})
	}
	return operations, nil
}
