// Package streambody owns producer-backed HTTP request bodies.
package streambody

import (
	"io"
	"sync"
)

// Body couples a pipe reader to its producer completion. Closing the consumer
// side is the cancellation signal: blocked producers observe io.ErrClosedPipe
// and can terminate instead of leaking after request setup fails.
type Body struct {
	reader    *io.PipeReader
	done      chan struct{}
	closers   []io.Closer
	closeOnce sync.Once
	closeErr  error
}

func New(produce func(io.Writer) error, closers ...io.Closer) *Body {
	reader, writer := io.Pipe()
	body := &Body{reader: reader, done: make(chan struct{}), closers: closers}
	go func() {
		_ = writer.CloseWithError(produce(writer))
		close(body.done)
	}()
	return body
}

func (b *Body) Read(payload []byte) (int, error) { return b.reader.Read(payload) }

func (b *Body) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.reader.Close()
		for _, closer := range b.closers {
			if err := closer.Close(); b.closeErr == nil && err != nil {
				b.closeErr = err
			}
		}
		// Joining here makes every HTTP setup or transport exit an ownership
		// boundary: no producer can outlive the request that consumed its body.
		<-b.done
	})
	return b.closeErr
}

// Done lets lifecycle tests and callers synchronize producer termination
// without polling goroutine counts.
func (b *Body) Done() <-chan struct{} { return b.done }
