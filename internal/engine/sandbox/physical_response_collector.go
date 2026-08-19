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

// PhysicalExecutionResult carries only the bounded canonical provider body
// consumed by the Unified interpreter.
type PhysicalExecutionResult struct {
	Body []byte
}

// boundedJSONResponseCollector deliberately implements only the dispatcher's
// response stream contracts. It never records or logs captured provider data.
type boundedJSONResponseCollector struct {
	body        bytes.Buffer
	status      int
	mediaFamily string
	err         error
}

// successfulResponseCollector discards compensation response bodies while
// retaining the provider status required to prove rollback success.
type successfulResponseCollector struct {
	status int
	err    error
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
		return fmt.Errorf("%w: HTTP %d", ErrPhysicalResponseStatus, collector.status)
	}
	return nil
}

// captureStatus ignores empty status frames and rejects conflicting nonzero statuses.
func (collector *successfulResponseCollector) captureStatus(status int) error {
	if status <= 0 {
		return nil
	}
	if collector.status != 0 && collector.status != status {
		collector.err = errors.New("physical response status changed during collection")
		return collector.err
	}
	collector.status = status
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
		return PhysicalExecutionResult{}, fmt.Errorf("%w: HTTP %d", ErrPhysicalResponseStatus, collector.status)
	}
	if collector.mediaFamily != "json" {
		return PhysicalExecutionResult{}, ErrPhysicalResponseNotJSON
	}
	canonical, err := canonicaljson.Canonicalize(collector.body.Bytes())
	if err != nil {
		return PhysicalExecutionResult{}, ErrPhysicalResponseNotJSON
	}
	return PhysicalExecutionResult{Body: bytes.Clone(canonical)}, nil
}

// captureStatus ignores empty status frames and rejects conflicting nonzero statuses.
func (collector *boundedJSONResponseCollector) captureStatus(status int) error {
	if status <= 0 {
		return nil
	}
	if collector.status != 0 && collector.status != status {
		collector.err = errors.New("physical response status changed during collection")
		return collector.err
	}
	collector.status = status
	return nil
}
