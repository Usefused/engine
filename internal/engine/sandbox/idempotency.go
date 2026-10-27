package sandbox

import (
	"context"
	"strings"
)

const (
	idempotencyHeaderName     = "Idempotency-Key"
	requestBodyHashHeaderName = "X-Fused-Request-Body-Hash"
)

type executionIdentityContextKey struct{}

type executionIdentity struct {
	idempotencyKey  string
	requestBodyHash string
}

func contextWithExecutionIdentity(ctx context.Context, idempotencyKey, requestBodyHash string) context.Context {
	id := executionIdentity{
		idempotencyKey:  strings.TrimSpace(idempotencyKey),
		requestBodyHash: strings.TrimSpace(requestBodyHash),
	}
	if id.idempotencyKey == "" && id.requestBodyHash == "" {
		return ctx
	}
	return context.WithValue(ctx, executionIdentityContextKey{}, id)
}

func idempotencyKeyFromContext(ctx context.Context) string {
	id, _ := ctx.Value(executionIdentityContextKey{}).(executionIdentity)
	return id.idempotencyKey
}

func requestBodyHashFromContext(ctx context.Context) string {
	id, _ := ctx.Value(executionIdentityContextKey{}).(executionIdentity)
	return id.requestBodyHash
}

// paramsWithExecutionHeaders copies caller input so retries hash the same authored parameters.
func paramsWithExecutionHeaders(params map[string]any, idempotencyKey, requestBodyHash string) map[string]any {
	// Unkeyed requests need no allocation or synthetic headers.
	if idempotencyKey == "" && requestBodyHash == "" {
		return params
	}
	out := make(map[string]any, len(params)+1)
	for key, value := range params {
		out[key] = value
	}
	headers := make(map[string]any)
	for key, value := range existingHeaders(params) {
		headers[key] = value
	}
	setHeaderIfMissing(headers, idempotencyHeaderName, idempotencyKey)
	setHeaderIfMissing(headers, requestBodyHashHeaderName, requestBodyHash)
	out["_headers"] = headers
	return out
}

func existingHeaders(params map[string]any) map[string]any {
	if headers, ok := params["_headers"].(map[string]any); ok {
		return headers
	}
	if headers, ok := params["_headers"].(map[string]string); ok {
		out := make(map[string]any, len(headers))
		for k, v := range headers {
			out[k] = v
		}
		return out
	}
	return map[string]any{}
}

func setHeaderIfMissing(headers map[string]any, key, value string) {
	if value == "" || hasHeader(headers, key) {
		return
	}
	headers[key] = value
}

func hasHeader(headers map[string]any, key string) bool {
	for existing := range headers {
		if strings.EqualFold(existing, key) {
			return true
		}
	}
	return false
}
