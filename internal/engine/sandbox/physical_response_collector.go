package sandbox

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
)

const maxPhysicalJSONResponseBytes = 1 << 20

var (
	ErrPhysicalResponseTooLarge = errors.New("physical response exceeded JSON body limit")
	ErrPhysicalResponseNotJSON  = errors.New("physical response is not one JSON document")
	ErrPhysicalResponseStatus   = errors.New("physical response status is not successful")
)

// PhysicalResponseStatusError carries only the provider's bounded numeric
// status so public adapters can offer safe retry guidance without response data.
type PhysicalResponseStatusError struct {
	StatusCode int
}

// Error returns a stable diagnostic containing no provider-controlled body.
func (err *PhysicalResponseStatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d", ErrPhysicalResponseStatus, err.StatusCode)
}

// Unwrap preserves sentinel classification for existing execution policies.
func (err *PhysicalResponseStatusError) Unwrap() error {
	return ErrPhysicalResponseStatus
}

// PhysicalExecutionResult carries only the bounded canonical provider body
// consumed by the Unified interpreter.
type PhysicalExecutionResult struct {
	Body       []byte
	StatusCode int
}

// statusCapture tracks one immutable provider HTTP status and any sticky
// collection error shared across response collectors.
type statusCapture struct {
	status int
	err    error
}

// captureStatus ignores empty status frames and rejects conflicting nonzero statuses.
func (capture *statusCapture) captureStatus(status int) error {
	// Stream frames without an HTTP status leave the recorded status unchanged.
	if status <= 0 {
		return nil
	}
	// Once observed, the response status must remain immutable across frames.
	if capture.status != 0 && capture.status != status {
		capture.err = errors.New("physical response status changed during collection")
		return capture.err
	}
	capture.status = status
	return nil
}

// boundedJSONResponseCollector deliberately implements only the dispatcher's
// response stream contracts. It never records or logs captured provider data.
type boundedJSONResponseCollector struct {
	statusCapture
	body        bytes.Buffer
	mediaFamily string
}

// successfulResponseCollector discards compensation response bodies while
// retaining the provider status required to prove rollback success.
type successfulResponseCollector struct {
	statusCapture
}

// Send deliberately discards rollback bodies because they are neither public
// output nor dependency input.
func (collector *successfulResponseCollector) Send([]byte) error {
	return collector.err
}

// SendStatus records the provider status without allowing later stream frames to change it.
func (collector *successfulResponseCollector) SendStatus(status int) error {
	return collector.captureStatus(status)
}

// SendResponseContract keeps only status because compensation bodies and media
// metadata are deliberately unobservable.
func (collector *successfulResponseCollector) SendResponseContract(status int, _ string) error {
	return collector.captureStatus(status)
}

// Result accepts any 2xx compensation, including bodyless delete responses.
func (collector *successfulResponseCollector) Result() error {
	if collector.err != nil {
		return collector.err
	}
	if collector.status < 200 || collector.status >= 300 {
		return &PhysicalResponseStatusError{StatusCode: collector.status}
	}
	return nil
}

// newBoundedJSONResponseCollector creates a fresh one-megabyte collector with sticky validation failures.
func newBoundedJSONResponseCollector() *boundedJSONResponseCollector {
	return &boundedJSONResponseCollector{}
}

// Send appends provider bytes only while the fixed response budget remains.
func (collector *boundedJSONResponseCollector) Send(chunk []byte) error {
	if collector.err != nil {
		return collector.err
	}
	if len(chunk) > maxPhysicalJSONResponseBytes-collector.body.Len() {
		collector.body.Reset()
		collector.err = ErrPhysicalResponseTooLarge
		return collector.err
	}
	_, err := collector.body.Write(chunk)
	return err
}

// SendStatus records the provider status without allowing later stream frames to change it.
func (collector *boundedJSONResponseCollector) SendStatus(status int) error {
	return collector.captureStatus(status)
}

// SendResponseContract captures both status and media family for final JSON validation.
func (collector *boundedJSONResponseCollector) SendResponseContract(status int, mediaFamily string) error {
	if err := collector.captureStatus(status); err != nil {
		return err
	}
	collector.mediaFamily = mediaFamily
	return nil
}

// Result accepts one successful canonical JSON document and returns a defensive body copy.
func (collector *boundedJSONResponseCollector) Result() (PhysicalExecutionResult, error) {
	if collector.err != nil {
		return PhysicalExecutionResult{}, collector.err
	}
	if collector.status < 200 || collector.status >= 300 {
		return PhysicalExecutionResult{}, &PhysicalResponseStatusError{StatusCode: collector.status}
	}
	if collector.mediaFamily != "json" {
		return PhysicalExecutionResult{}, ErrPhysicalResponseNotJSON
	}
	canonical, err := canonicaljson.Canonicalize(collector.body.Bytes())
	if err != nil {
		return PhysicalExecutionResult{}, ErrPhysicalResponseNotJSON
	}
	return PhysicalExecutionResult{Body: bytes.Clone(canonical), StatusCode: collector.status}, nil
}
