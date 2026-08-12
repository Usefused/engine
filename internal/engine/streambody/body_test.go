package streambody

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloseUnblocksProducer(t *testing.T) {
	var completed atomic.Bool
	body := New(func(destination io.Writer) error {
		defer completed.Store(true)
		for {
			if _, err := destination.Write(make([]byte, 1024)); err != nil {
				return err
			}
		}
	})
	if err := body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !completed.Load() {
		t.Fatal("Close returned before producer completion")
	}
	select {
	case <-body.Done():
	case <-time.After(time.Second):
		t.Fatal("producer remained blocked after body close")
	}
}

func TestCloseCancelsOwnedSourceAndIsConcurrentSafe(t *testing.T) {
	source := newBlockingReadCloser()
	body := New(func(destination io.Writer) error {
		_, err := io.Copy(destination, source)
		return err
	}, source)

	errorsSeen := make(chan error, 2)
	for range 2 {
		go func() { errorsSeen <- body.Close() }()
	}
	for range 2 {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if source.closeCount.Load() != 1 {
		t.Fatalf("source close count = %d", source.closeCount.Load())
	}
}

type blockingReadCloser struct {
	closed     chan struct{}
	closeOnce  sync.Once
	closeCount atomic.Int32
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.closed
	return 0, errors.New("source closed")
}

func (r *blockingReadCloser) Close() error {
	r.closeOnce.Do(func() {
		r.closeCount.Add(1)
		close(r.closed)
	})
	return nil
}
