package observability

import "strings"

func isOTLPEndpointURL(endpoint string) bool {
	return strings.Contains(endpoint, "://")
}
