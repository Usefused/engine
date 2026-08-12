// Package uploadworkflow executes provider-neutral, reviewed media workflows.
package uploadworkflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/Usefused/engine/internal/engine/streambody"
	"github.com/Usefused/engine/internal/shared/workflowcontract"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const maxMetadataBytes = 2 << 20

type Input struct {
	Mode       workflowcontract.UploadModeKind
	BaseURL    string
	Metadata   []byte
	Media      io.Reader
	MediaSize  int64
	MediaType  string
	ChunkBytes int64
}

type AttemptFunc func(context.Context, *http.Request, func(*url.URL) error) (*http.Response, error)

// Executor delegates every provider mutation to the canonical Engine attempt
// path, which owns auth, quota/concurrency, retry, and execution timing.
type Executor struct{ Attempt AttemptFunc }

// Execute streams media without buffering the whole upload. Only one bounded
// chunk is held because resumable providers require an exact Content-Range.
func (e Executor) Execute(ctx context.Context, workflow *workflowcontract.UploadWorkflow, input Input) (*http.Response, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.upload_workflow.execute")
	defer span.End()
	mode, err := selectMode(workflow, input)
	if err != nil {
		span.SetStatus(codes.Error, "workflow_rejected")
		return nil, err
	}
	span.SetAttributes(attribute.String("upload.mode", string(mode.Kind)))
	response, err := e.executeMode(ctx, mode, input)
	if err != nil {
		span.SetStatus(codes.Error, "workflow_failed")
		return nil, err
	}
	span.SetStatus(codes.Ok, "workflow_complete")
	return response, nil
}

func selectMode(workflow *workflowcontract.UploadWorkflow, input Input) (*workflowcontract.UploadMode, error) {
	if err := validateUploadInput(workflow, input); err != nil {
		return nil, err
	}
	for index := range workflow.Modes {
		if workflow.Modes[index].Kind == input.Mode {
			return &workflow.Modes[index], nil
		}
	}
	return nil, errors.New("upload mode is not declared")
}

func validateUploadInput(workflow *workflowcontract.UploadWorkflow, input Input) error {
	if workflowcontract.Validate(workflow) != nil || input.Media == nil || input.MediaSize <= 0 {
		return errors.New("upload workflow input is invalid")
	}
	if len(input.Metadata) > maxMetadataBytes {
		return errors.New("upload metadata exceeds maximum")
	}
	if workflow.MaxSizeBytes > 0 && input.MediaSize > workflow.MaxSizeBytes {
		return errors.New("media exceeds workflow maximum")
	}
	if !mediaAccepted(workflow.AcceptedMediaTypes, input.MediaType) {
		return errors.New("media type is not accepted")
	}
	return nil
}

func mediaAccepted(accepted []string, value string) bool {
	for _, candidate := range accepted {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

func (e Executor) executeMode(ctx context.Context, mode *workflowcontract.UploadMode, input Input) (*http.Response, error) {
	if len(mode.Steps) == 1 {
		return e.executeDirect(ctx, mode.Steps[0], input)
	}
	if len(mode.Steps) != 2 {
		return nil, errors.New("upload workflow step sequence is invalid")
	}
	initResponse, err := e.executeInitiation(ctx, mode.Steps[0], input)
	if err != nil {
		return nil, err
	}
	defer initResponse.Body.Close()
	transferURL, err := responseURL(initResponse, mode.Steps[1].URL, input.BaseURL)
	if err != nil {
		return nil, err
	}
	return e.executeChunks(ctx, mode.Steps[1], transferURL, input)
}

func (e Executor) executeDirect(ctx context.Context, step workflowcontract.UploadStep, input Input) (*http.Response, error) {
	target, err := declaredURL(input.BaseURL, step.URL.Path)
	if err != nil {
		return nil, err
	}
	body, contentType, err := directBody(step.Body, input)
	if err != nil {
		return nil, err
	}
	return e.do(ctx, step, target, body, contentType, input.MediaSize, input.BaseURL)
}

func (e Executor) executeInitiation(ctx context.Context, step workflowcontract.UploadStep, input Input) (*http.Response, error) {
	target, err := declaredURL(input.BaseURL, step.URL.Path)
	if err != nil {
		return nil, err
	}
	return e.do(ctx, step, target, bytes.NewReader(input.Metadata), "application/json", int64(len(input.Metadata)), input.BaseURL)
}

func directBody(kind workflowcontract.UploadBodyKind, input Input) (io.Reader, string, error) {
	switch kind {
	case workflowcontract.BodyMedia:
		return input.Media, input.MediaType, nil
	case workflowcontract.BodyMetadata:
		return bytes.NewReader(input.Metadata), "application/json", nil
	case workflowcontract.BodyMultipart:
		return multipartBody(input)
	default:
		return nil, "", errors.New("upload body kind is unsupported")
	}
}

func multipartBody(input Input) (io.Reader, string, error) {
	prototype := multipart.NewWriter(io.Discard)
	var closers []io.Closer
	if closer, ok := input.Media.(io.Closer); ok {
		closers = append(closers, closer)
	}
	body := streambody.New(func(destination io.Writer) error {
		writer := multipart.NewWriter(destination)
		if err := writer.SetBoundary(prototype.Boundary()); err != nil {
			return errors.New("failed to set upload multipart boundary")
		}
		if err := writeMultipart(writer, input); err != nil {
			return err
		}
		return writer.Close()
	}, closers...)
	return body, prototype.FormDataContentType(), nil
}

func writeMultipart(writer *multipart.Writer, input Input) error {
	metadataHeader := textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}}
	metadataPart, err := writer.CreatePart(metadataHeader)
	if err != nil {
		return err
	}
	if _, err := metadataPart.Write(input.Metadata); err != nil {
		return err
	}
	mediaHeader := textproto.MIMEHeader{"Content-Type": {input.MediaType}}
	mediaPart, err := writer.CreatePart(mediaHeader)
	if err != nil {
		return err
	}
	_, err = io.Copy(mediaPart, input.Media)
	return err
}

func (e Executor) executeChunks(ctx context.Context, step workflowcontract.UploadStep, target string, input Input) (*http.Response, error) {
	chunkSize, err := selectedChunkSize(step.Chunking, input.ChunkBytes)
	if err != nil {
		return nil, err
	}
	buffer := make([]byte, chunkSize)
	var offset int64
	for offset < input.MediaSize {
		expected := minInt64(chunkSize, input.MediaSize-offset)
		count, readErr := io.ReadFull(input.Media, buffer[:expected])
		// Never mutate the provider with a partial chunk when the caller's
		// declared size exceeds the available media stream.
		if readErr != nil || int64(count) != expected {
			return nil, errors.New("media stream ended before declared size")
		}
		response, err := e.doChunk(ctx, step, target, buffer[:count], offset, input)
		if err != nil {
			return nil, err
		}
		offset += int64(count)
		done, err := classifyChunkResponse(response, step, offset == input.MediaSize, count)
		if err != nil {
			return nil, err
		}
		if done {
			return response, nil
		}
	}
	return nil, errors.New("upload ended without a success response")
}

func classifyChunkResponse(response *http.Response, step workflowcontract.UploadStep, complete bool, count int) (bool, error) {
	if statusIn(response.StatusCode, step.SuccessStatuses) && complete {
		return true, nil
	}
	defer response.Body.Close()
	if statusIn(response.StatusCode, step.SuccessStatuses) {
		return false, errors.New("provider completed upload before all media was transferred")
	}
	if !statusIn(response.StatusCode, step.ContinueStatuses) || count == 0 {
		return false, fmt.Errorf("upload transfer returned status %d", response.StatusCode)
	}
	return false, nil
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func selectedChunkSize(chunking *workflowcontract.Chunking, requested int64) (int64, error) {
	if chunking == nil {
		return 0, errors.New("resumable transfer requires chunking")
	}
	size := requested
	if size == 0 {
		size = chunking.DefaultSizeBytes
	}
	if size < 1 || size > chunking.MaxSizeBytes || size%chunking.SizeMultipleBytes != 0 {
		return 0, errors.New("upload chunk size is invalid")
	}
	return size, nil
}

func (e Executor) doChunk(ctx context.Context, step workflowcontract.UploadStep, target string, body []byte, offset int64, input Input) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, step.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", input.MediaType)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(body))-1, input.MediaSize))
	return e.doRequest(req, step, input.BaseURL)
}

func (e Executor) do(ctx context.Context, step workflowcontract.UploadStep, target string, body io.Reader, contentType string, size int64, baseURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, step.Method, target, body)
	if err != nil {
		closeReader(body)
		return nil, err
	}
	defer req.Body.Close()
	req.Header.Set("Content-Type", contentType)
	if size >= 0 && step.Body != workflowcontract.BodyMultipart {
		req.ContentLength = size
	}
	response, err := e.doRequest(req, step, baseURL)
	if err != nil {
		return nil, err
	}
	if !statusIn(response.StatusCode, step.SuccessStatuses) {
		_ = response.Body.Close()
		return nil, fmt.Errorf("upload step returned status %d", response.StatusCode)
	}
	return response, nil
}

func closeReader(reader io.Reader) {
	if closer, ok := reader.(io.Closer); ok {
		_ = closer.Close()
	}
}

func (e Executor) doRequest(req *http.Request, step workflowcontract.UploadStep, baseURL string) (*http.Response, error) {
	if e.Attempt == nil {
		return nil, errors.New("canonical provider attempt is unavailable")
	}
	guard := func(candidate *url.URL) error {
		if !originAllowed(candidate, step.URL.AllowedOrigins, baseURL) {
			return errors.New("upload redirect origin is not allowed")
		}
		return nil
	}
	return e.Attempt(req.Context(), req, guard)
}

func declaredURL(baseURL, path string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return "", errors.New("upload base URL is invalid")
	}
	reference, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(path, "/") {
		return "", errors.New("upload path is invalid")
	}
	return base.ResolveReference(reference).String(), nil
}

func responseURL(response *http.Response, source workflowcontract.URLSource, baseURL string) (string, error) {
	raw := response.Header.Get(source.HeaderName)
	parsed, err := url.Parse(raw)
	if err != nil || raw == "" {
		return "", errors.New("upload response URL is missing or invalid")
	}
	resolved := response.Request.URL.ResolveReference(parsed)
	if !originAllowed(resolved, source.AllowedOrigins, baseURL) {
		return "", errors.New("upload response URL origin is not allowed")
	}
	return resolved.String(), nil
}

func originAllowed(candidate *url.URL, allowed []string, baseURL string) bool {
	if candidate == nil || candidate.User != nil || candidate.Scheme == "" || candidate.Host == "" {
		return false
	}
	wanted := normalizedOrigin(candidate)
	if len(allowed) == 0 {
		base, err := url.Parse(baseURL)
		return err == nil && wanted == normalizedOrigin(base)
	}
	for _, value := range allowed {
		parsed, err := url.Parse(value)
		if err == nil && wanted == normalizedOrigin(parsed) {
			return true
		}
	}
	return false
}

func normalizedOrigin(value *url.URL) string {
	host := strings.ToLower(value.Hostname())
	port := value.Port()
	if port != "" && !((value.Scheme == "https" && port == "443") || (value.Scheme == "http" && port == "80")) {
		host += ":" + port
	}
	return strings.ToLower(value.Scheme) + "://" + host
}

func statusIn(status int, ranges []workflowcontract.StatusRange) bool {
	for _, current := range ranges {
		if status >= current.Min && status <= current.Max {
			return true
		}
	}
	return false
}
