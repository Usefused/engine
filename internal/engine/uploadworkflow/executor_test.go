package uploadworkflow_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"testing"

	"github.com/Usefused/engine/internal/engine/uploadworkflow"
	"github.com/Usefused/engine/internal/shared/workflowcontract"
	"github.com/Usefused/engine/internal/testcontract"
)

// loadWorkflow centralizes the provider-neutral reviewed workflow used by all
// execution-mode tests so they cannot drift onto hand-built variants.
func loadWorkflow(t *testing.T) workflowcontract.UploadWorkflow {
	t.Helper()
	return testcontract.UploadWorkflow()
}

// TestResumableWorkflowUsesBoundedChunksAndExactStatuses uses the shared
// provider-neutral route so execution tests remain coupled to contract behavior
// without restoring provider-specific paths removed from the fixture corpus.
func TestResumableWorkflowUsesBoundedChunksAndExactStatuses(t *testing.T) {
	workflow := loadWorkflow(t)
	media := bytes.Repeat([]byte("x"), 524288)
	requests := 0
	attempt := reviewedUploadDouble(t, &requests)
	result, err := (uploadworkflow.Executor{Attempt: attempt}).Execute(t.Context(), &workflow, uploadworkflow.Input{
		Mode: workflowcontract.UploadResumable, BaseURL: "https://api.example.test", Metadata: []byte(`{"name":"file"}`),
		Media: bytes.NewReader(media), MediaSize: int64(len(media)), MediaType: "application/octet-stream", ChunkBytes: 262144,
	})
	if err != nil || result.StatusCode != 201 || requests != 3 {
		t.Fatalf("result=%v requests=%d err=%v", result, requests, err)
	}
	_ = result.Body.Close()
}

// reviewedUploadDouble returns only origins permitted by the shared workflow,
// which keeps this double focused on origin enforcement rather than a provider.
func reviewedUploadDouble(t *testing.T, requests *int) uploadworkflow.AttemptFunc {
	t.Helper()
	return func(_ context.Context, request *http.Request, guard func(*url.URL) error) (*http.Response, error) {
		if err := guard(request.URL); err != nil {
			return nil, err
		}
		*requests++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if *requests == 1 {
			assertInitiation(t, request, body)
			return response(request, 200, map[string]string{"Location": "https://uploads.example.test/session/1"}), nil
		}
		if *requests == 2 {
			assertChunk(t, request, body, "bytes 0-262143/524288")
			return response(request, 308, nil), nil
		}
		assertChunk(t, request, body, "bytes 262144-524287/524288")
		return response(request, 201, nil), nil
	}
}

// assertInitiation checks the canonical declared path because accepting the
// former provider route would let fixture and executor coverage drift apart.
func assertInitiation(t *testing.T, request *http.Request, body []byte) {
	t.Helper()
	if request.URL.Path != "/resumable/upload/v1/files" || string(body) != `{"name":"file"}` {
		t.Fatalf("initiation request = %s %q", request.URL, body)
	}
}

func assertChunk(t *testing.T, request *http.Request, body []byte, expectedRange string) {
	t.Helper()
	if len(body) != 262144 || request.Header.Get("Content-Range") != expectedRange {
		t.Fatalf("chunk size=%d range=%q", len(body), request.Header.Get("Content-Range"))
	}
}

// TestResumableWorkflowRejectsUnreviewedResponseOrigin proves response-controlled upload locations remain allowlisted.
func TestResumableWorkflowRejectsUnreviewedResponseOrigin(t *testing.T) {
	workflow := loadWorkflow(t)
	attempt := func(_ context.Context, request *http.Request, _ func(*url.URL) error) (*http.Response, error) {
		return response(request, 200, map[string]string{"Location": "https://attacker.invalid/session"}), nil
	}
	_, err := (uploadworkflow.Executor{Attempt: attempt}).Execute(t.Context(), &workflow, uploadworkflow.Input{
		Mode: workflowcontract.UploadResumable, BaseURL: "https://api.example.test", Metadata: []byte(`{}`),
		Media: bytes.NewReader([]byte("x")), MediaSize: 1, MediaType: "application/octet-stream",
	})
	if err == nil {
		t.Fatal("expected unreviewed Location origin rejection")
	}
}

func TestResumableWorkflowRejectsShortMediaBeforeTransfer(t *testing.T) {
	workflow := tinyResumableWorkflow()
	requests := 0
	attempt := func(_ context.Context, request *http.Request, _ func(*url.URL) error) (*http.Response, error) {
		requests++
		return response(request, 200, map[string]string{"Location": "https://upload.example/session"}), nil
	}
	_, err := (uploadworkflow.Executor{Attempt: attempt}).Execute(t.Context(), workflow, uploadworkflow.Input{
		Mode: workflowcontract.UploadResumable, BaseURL: "https://upload.example", Metadata: []byte(`{}`),
		Media: bytes.NewReader([]byte("short")), MediaSize: 8, MediaType: "application/octet-stream", ChunkBytes: 4,
	})
	if err == nil || requests != 2 {
		t.Fatalf("short media err=%v provider requests=%d", err, requests)
	}
}

func TestResumableWorkflowRejectsEarlySuccess(t *testing.T) {
	workflow := tinyResumableWorkflow()
	requests := 0
	attempt := func(_ context.Context, request *http.Request, _ func(*url.URL) error) (*http.Response, error) {
		requests++
		if requests == 1 {
			return response(request, 200, map[string]string{"Location": "https://upload.example/session"}), nil
		}
		return response(request, 200, nil), nil
	}
	_, err := (uploadworkflow.Executor{Attempt: attempt}).Execute(t.Context(), workflow, uploadworkflow.Input{
		Mode: workflowcontract.UploadResumable, BaseURL: "https://upload.example", Metadata: []byte(`{}`),
		Media: bytes.NewReader([]byte("12345678")), MediaSize: 8, MediaType: "application/octet-stream", ChunkBytes: 4,
	})
	if err == nil || requests != 2 {
		t.Fatalf("early success err=%v provider requests=%d", err, requests)
	}
}

func TestMultipartWorkflowJoinsProducerAfterPreTransportFailure(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 32 {
		source := newSignalReadCloser()
		executor := uploadworkflow.Executor{Attempt: func(context.Context, *http.Request, func(*url.URL) error) (*http.Response, error) {
			return nil, errors.New("rate limit rejected request")
		}}
		_, err := executor.Execute(t.Context(), multipartWorkflow(), multipartInput(source))
		if err == nil {
			t.Fatal("expected pre-transport failure")
		}
		assertSourceClosed(t, source)
	}
	runtime.Gosched()
	if delta := runtime.NumGoroutine() - before; delta > 4 {
		t.Fatalf("streaming failures leaked %d goroutines", delta)
	}
}

func TestMultipartWorkflowJoinsProducerAfterContextCancellation(t *testing.T) {
	source := newSignalReadCloser()
	ctx, cancel := context.WithCancel(t.Context())
	executor := uploadworkflow.Executor{Attempt: func(attemptCtx context.Context, _ *http.Request, _ func(*url.URL) error) (*http.Response, error) {
		cancel()
		<-attemptCtx.Done()
		return nil, attemptCtx.Err()
	}}
	_, err := executor.Execute(ctx, multipartWorkflow(), multipartInput(source))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	assertSourceClosed(t, source)
}

func multipartWorkflow() *workflowcontract.UploadWorkflow {
	return &workflowcontract.UploadWorkflow{Version: workflowcontract.Version, AcceptedMediaTypes: []string{"application/octet-stream"}, Modes: []workflowcontract.UploadMode{{
		Kind: workflowcontract.UploadMultipart, Steps: []workflowcontract.UploadStep{{
			Kind: workflowcontract.StepTransfer, Method: http.MethodPost,
			URL:  workflowcontract.URLSource{Kind: workflowcontract.URLDeclaredPath, Path: "/upload"},
			Body: workflowcontract.BodyMultipart, SuccessStatuses: []workflowcontract.StatusRange{{Min: 200, Max: 299}},
		}},
	}}}
}

func multipartInput(source io.Reader) uploadworkflow.Input {
	return uploadworkflow.Input{Mode: workflowcontract.UploadMultipart, BaseURL: "https://upload.example", Metadata: []byte(`{"name":"file"}`),
		Media: source, MediaSize: 1, MediaType: "application/octet-stream"}
}

type signalReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newSignalReadCloser() *signalReadCloser {
	return &signalReadCloser{closed: make(chan struct{})}
}

func (r *signalReadCloser) Read([]byte) (int, error) {
	<-r.closed
	return 0, errors.New("source closed")
}

func (r *signalReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func assertSourceClosed(t *testing.T, source *signalReadCloser) {
	t.Helper()
	select {
	case <-source.closed:
	default:
		t.Fatal("stream source remained open after request completion")
	}
}

func tinyResumableWorkflow() *workflowcontract.UploadWorkflow {
	return &workflowcontract.UploadWorkflow{Version: 1, AcceptedMediaTypes: []string{"application/octet-stream"}, Modes: []workflowcontract.UploadMode{{
		Kind: workflowcontract.UploadResumable, Steps: []workflowcontract.UploadStep{
			{Kind: workflowcontract.StepInitiate, Method: "POST", URL: workflowcontract.URLSource{Kind: workflowcontract.URLDeclaredPath, Path: "/init"}, Body: workflowcontract.BodyMetadata, SuccessStatuses: []workflowcontract.StatusRange{{Min: 200, Max: 200}}},
			{Kind: workflowcontract.StepTransfer, Method: "PUT", URL: workflowcontract.URLSource{Kind: workflowcontract.URLResponseHeader, HeaderName: "Location"}, Body: workflowcontract.BodyMedia, Chunking: &workflowcontract.Chunking{DefaultSizeBytes: 4, SizeMultipleBytes: 1, MaxSizeBytes: 4}, SuccessStatuses: []workflowcontract.StatusRange{{Min: 200, Max: 200}}, ContinueStatuses: []workflowcontract.StatusRange{{Min: 308, Max: 308}}},
		},
	}}}
}

func response(request *http.Request, status int, headers map[string]string) *http.Response {
	header := make(http.Header)
	for name, value := range headers {
		header.Set(name, value)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}
}
