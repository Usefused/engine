package engine

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
)

const maxDeferredResponseBytes = 1 << 20

var errDeferredResponseTooLarge = errors.New("retryable provider response exceeded deferred body limit")

// ErrBufferStreamLimitExceeded is the stable failure returned when a bounded
// response consumer reaches its admitted in-memory result budget.
var ErrBufferStreamLimitExceeded = errors.New("buffered response limit exceeded")

// ResponseStream streams execution result chunks back to the caller — either the
// gRPC edge (relaying to the open stream) or a buffering adapter.
type ResponseStream interface {
	Send(chunk []byte) error
}

// ResponseStatusStream is implemented by transports that can preserve the
// provider's HTTP status separately from its streamed body.
type ResponseStatusStream interface {
	SendStatus(status int) error
}

// ResponseContractStream publishes the bounded response facts a generated
// client needs before consuming body chunks. Exact media types stay inside the
// provider boundary and never enter transport metadata or telemetry.
type ResponseContractStream interface {
	SendResponseContract(status int, mediaFamily string) error
}

// SendResponseStatus keeps status propagation optional for buffered consumers
// such as MCP while allowing SDK transports to retain provider semantics.
func SendResponseStatus(stream ResponseStream, status int) error {
	if statusStream, ok := stream.(ResponseStatusStream); ok && status > 0 {
		return statusStream.SendStatus(status)
	}
	return nil
}

// SendResponseContract keeps exact provider media private and emits only the
// bounded family needed for generated-client decoding.
func SendResponseContract(stream ResponseStream, status int, mediaFamily string) error {
	contractStream, ok := stream.(ResponseContractStream)
	if !ok {
		return nil
	}
	return contractStream.SendResponseContract(status, boundedResponseContractFamily(mediaFamily))
}

func boundedResponseContractFamily(family string) string {
	switch family {
	case "sse", "json", "binary", "xml", "text", "other", "unknown":
		return family
	default:
		return "unknown"
	}
}

// deferredResponseStream keeps a retryable attempt private until the retry
// loop decides it is the logical response. Non-retryable responses pass
// through immediately, so successful SSE streams retain live item delivery.
type deferredResponseStream struct {
	inner       ResponseStream
	shouldDefer func(int) bool
	buffer      bytes.Buffer
	status      int
	family      string
	deferred    bool
	committed   bool
	headers     http.Header
	requestURL  *url.URL
	bufferErr   error
}

func newDeferredResponseStream(inner ResponseStream, shouldDefer func(int) bool) *deferredResponseStream {
	return &deferredResponseStream{inner: inner, shouldDefer: shouldDefer}
}

func (stream *deferredResponseStream) SendResponseContract(status int, family string) error {
	stream.status, stream.family = status, family
	stream.deferred = stream.shouldDefer != nil && stream.shouldDefer(status)
	if stream.deferred {
		return nil
	}
	stream.committed = true
	return SendResponseContract(stream.inner, status, family)
}

func (stream *deferredResponseStream) Send(chunk []byte) error {
	if stream.deferred && !stream.committed {
		if stream.buffer.Len()+len(chunk) > maxDeferredResponseBytes {
			// Only an attempt that may be discarded is buffered. A hard cap keeps
			// an upstream retry response from turning policy evaluation into an
			// unbounded allocation; the final attempt still streams directly.
			stream.buffer.Reset()
			stream.bufferErr = errDeferredResponseTooLarge
			return stream.bufferErr
		}
		_, err := stream.buffer.Write(chunk)
		return err
	}
	return stream.inner.Send(chunk)
}

func (stream *deferredResponseStream) Commit() error {
	if stream.committed || !stream.deferred {
		return nil
	}
	if stream.bufferErr != nil {
		return stream.bufferErr
	}
	if err := SendResponseContract(stream.inner, stream.status, stream.family); err != nil {
		return err
	}
	stream.committed = true
	stream.forwardCapturedMetadata()
	if stream.buffer.Len() == 0 {
		return nil
	}
	return stream.inner.Send(stream.buffer.Bytes())
}

func (stream *deferredResponseStream) ResetForRetry() {
	stream.buffer.Reset()
	stream.status, stream.family = 0, ""
	stream.deferred, stream.committed = false, false
	stream.headers, stream.requestURL = nil, nil
	stream.bufferErr = nil
}

func (stream *deferredResponseStream) CaptureResponseMetadata(headers http.Header, requestURL *url.URL) {
	if stream.deferred && !stream.committed {
		// Pagination and retry consumers must observe metadata from the same
		// logical response as the published status, family, and body.
		stream.headers = headers.Clone()
		if requestURL != nil {
			cloned := *requestURL
			stream.requestURL = &cloned
		}
		return
	}
	stream.forwardMetadata(headers, requestURL)
}

func (stream *deferredResponseStream) forwardCapturedMetadata() {
	if stream.headers != nil || stream.requestURL != nil {
		stream.forwardMetadata(stream.headers, stream.requestURL)
	}
}

func (stream *deferredResponseStream) forwardMetadata(headers http.Header, requestURL *url.URL) {
	if sink, ok := stream.inner.(interface {
		CaptureResponseMetadata(http.Header, *url.URL)
	}); ok {
		sink.CaptureResponseMetadata(headers, requestURL)
	}
}

// BufferStream is an in-memory ResponseStream. It is the single buffering
// implementation shared by both callers that need one: the dispatcher's
// pagination loop (which reads each page's bytes) and the MCP path (which needs
// the full response as a buffered string). Having one type here keeps those
// callers DRY instead of each defining its own buffer.
type BufferStream struct {
	buf      bytes.Buffer
	status   int
	maxBytes int
	limited  bool
	err      error
}

// NewBufferStream returns an empty in-memory ResponseStream.
func NewBufferStream() *BufferStream { return &BufferStream{} }

// NewBoundedBufferStream returns an in-memory stream that rejects output past
// maxBytes without retaining a partial result for a caller to consume.
func NewBoundedBufferStream(maxBytes int) *BufferStream {
	// A negative budget cannot quietly restore unbounded buffering at a security boundary.
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &BufferStream{maxBytes: maxBytes, limited: true}
}

// Send appends a chunk while preserving a sticky bounded-buffer failure.
func (b *BufferStream) Send(chunk []byte) error {
	// Once a result crosses its budget, later chunks must not recreate a partial response.
	if b.err != nil {
		return b.err
	}
	// Checking the remaining capacity before Write avoids allocating the oversized chunk.
	if b.limited && len(chunk) > b.maxBytes-b.buf.Len() {
		b.buf = bytes.Buffer{}
		b.err = ErrBufferStreamLimitExceeded
		return b.err
	}
	_, err := b.buf.Write(chunk)
	return err
}

// Bytes returns the accumulated payload.
func (b *BufferStream) Bytes() []byte { return b.buf.Bytes() }

// String returns the accumulated payload as a string.
func (b *BufferStream) String() string { return b.buf.String() }

// SendResponseContract retains status separately so buffered adapters cannot mistake provider errors for success.
func (b *BufferStream) SendResponseContract(status int, _ string) error {
	b.status = status
	return nil
}

// SendStatus preserves status from transports that publish only the HTTP status contract.
func (b *BufferStream) SendStatus(status int) error {
	b.status = status
	return nil
}

// Status returns the final provider status without inferring success from response JSON.
func (b *BufferStream) Status() int { return b.status }
