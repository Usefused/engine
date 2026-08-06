package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type executionTimeoutError struct {
	Code      string `json:"code"`
	TimeoutMs int    `json:"timeout_ms"`
}

// A distinct cause lets the Engine distinguish its policy budget from an
// earlier SDK or upstream deadline without inferring ownership from error text.
var errExecutionPolicyDeadline = errors.New("execution policy deadline exceeded")

func newExecutionTimeoutError(timeoutMs int) *executionTimeoutError {
	return &executionTimeoutError{Code: "execution_timeout", TimeoutMs: timeoutMs}
}

func (e *executionTimeoutError) Error() string {
	return fmt.Sprintf("execution timed out after %dms", e.TimeoutMs)
}

func (e *executionTimeoutError) Unwrap() error {
	return context.DeadlineExceeded
}

func contextWithExecutionPolicyTimeout(ctx context.Context, timeoutMs *int, span trace.Span) (context.Context, context.CancelFunc, int) {
	if timeoutMs == nil {
		return ctx, func() {}, 0
	}
	span.SetAttributes(attribute.Int("execution.timeout_ms", *timeoutMs))
	bounded, cancel := context.WithTimeoutCause(ctx, time.Duration(*timeoutMs)*time.Millisecond, errExecutionPolicyDeadline)
	return bounded, cancel, *timeoutMs
}

func normalizeExecutionTimeout(ctx context.Context, err error, timeoutMs int) error {
	if timeoutMs > 0 && err != nil && errors.Is(context.Cause(ctx), errExecutionPolicyDeadline) {
		return newExecutionTimeoutError(timeoutMs)
	}
	return err
}
