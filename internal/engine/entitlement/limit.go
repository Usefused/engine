package entitlement

import (
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// LimitExceeded is returned when a hard ceiling is reached.
type LimitExceeded struct {
	Resource string
	Current  int
	Limit    int
}

func (e *LimitExceeded) Error() string {
	if e.Limit == 0 {
		return fmt.Sprintf("%s creation not allowed", e.Resource)
	}
	return fmt.Sprintf("%s limit reached (%d/%d)", e.Resource, e.Current, e.Limit)
}

// CheckLimit evaluates whether adding one more of resource would exceed the
// account's entitlement. Negative limit (-1) means unlimited. Zero means
// explicitly disallowed. Positive values are hard ceilings.
//
// The limit argument is a *int so nil (missing/unknown) can be treated as
// unlimited without conflating it with the explicit zero value.
func CheckLimit(span trace.Span, resource string, currentCount int, limit *int) *LimitExceeded {
	if limit == nil {
		span.SetAttributes(
			attribute.Int("entitlement."+resource+".current", currentCount),
			attribute.Int("entitlement."+resource+".limit", -1),
		)
		return nil
	}
	span.SetAttributes(
		attribute.Int("entitlement."+resource+".current", currentCount),
		attribute.Int("entitlement."+resource+".limit", *limit),
	)
	if *limit < 0 {
		return nil
	}
	if *limit == 0 {
		return &LimitExceeded{Resource: resource, Current: currentCount, Limit: 0}
	}
	if currentCount >= *limit {
		return &LimitExceeded{Resource: resource, Current: currentCount, Limit: *limit}
	}
	return nil
}
