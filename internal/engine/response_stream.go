package engine

import "bytes"

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

// SendResponseStatus keeps status propagation optional for buffered consumers
// such as MCP while allowing SDK transports to retain provider semantics.
func SendResponseStatus(stream ResponseStream, status int) error {
	if statusStream, ok := stream.(ResponseStatusStream); ok && status > 0 {
		return statusStream.SendStatus(status)
	}
	return nil
}

// BufferStream is an in-memory ResponseStream. It is the single buffering
// implementation shared by both callers that need one: the dispatcher's
// pagination loop (which reads each page's bytes) and the MCP path (which needs
// the full response as a buffered string). Having one type here keeps those
// callers DRY instead of each defining its own buffer.
type BufferStream struct {
	buf bytes.Buffer
}

// NewBufferStream returns an empty in-memory ResponseStream.
func NewBufferStream() *BufferStream { return &BufferStream{} }

func (b *BufferStream) Send(chunk []byte) error {
	_, err := b.buf.Write(chunk)
	return err
}

// Bytes returns the accumulated payload.
func (b *BufferStream) Bytes() []byte { return b.buf.Bytes() }

// String returns the accumulated payload as a string.
func (b *BufferStream) String() string { return b.buf.String() }
