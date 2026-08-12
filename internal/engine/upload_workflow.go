package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/uploadworkflow"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/workflowcontract"
	"go.opentelemetry.io/otel/trace"
)

func (d *Dispatcher) executeUploadWorkflow(ctx context.Context, srv *models.Service, obj *models.IntegrationObject, params, credentials map[string]any, bucketValues []store.BucketValue, stream ResponseStream) (int, error) {
	input, err := uploadInput(obj.RequestContent, params, bindingBaseURL(srv.BaseURL, bucketValues))
	if err != nil {
		return 0, err
	}
	selectedAuths, err := selectRequestAuth(srv.AuthConfigs, obj.SecurityRequirements, credentials)
	if err != nil {
		return 0, err
	}
	if err := applySelectedSecurityServer(srv, obj, selectedAuths, bucketValues); err != nil {
		return 0, err
	}
	input.BaseURL = bindingBaseURL(srv.BaseURL, bucketValues)
	client, err := d.providerClientForAuth(selectedAuths, credentials)
	if err != nil {
		return 0, err
	}
	runner := uploadworkflow.Executor{Attempt: d.uploadAttempt(srv, obj, selectedAuths, credentials, client)}
	response, err := runner.Execute(ctx, obj.RequestContent.UploadWorkflow, input)
	if err != nil {
		return 0, err
	}
	// Upload attempts already account for quota response signals. Reusing the
	// canonical completion boundary here preserves actual status/media selection
	// and SSE framing without charging the final provider response twice.
	return d.completeProviderAttempt(ctx, trace.SpanFromContext(ctx), srv, obj, response, stream, false)
}

func uploadInput(content *models.RequestContent, params map[string]any, baseURL string) (uploadworkflow.Input, error) {
	selected, _, err := SelectRequestContent(content)
	if err != nil {
		return uploadworkflow.Input{}, err
	}
	media, mediaSize, err := uploadMedia(selected, params)
	if err != nil {
		return uploadworkflow.Input{}, err
	}
	metadata, err := json.Marshal(params["metadata"])
	if err != nil {
		return uploadworkflow.Input{}, errors.New("upload metadata is invalid")
	}
	if params["metadata"] == nil {
		metadata = []byte("{}")
	}
	mode, _ := params["upload_mode"].(string)
	chunk, _ := numericInt64(params["chunk_size_bytes"])
	return uploadworkflow.Input{Mode: uploadworkflowMode(mode), BaseURL: baseURL, Metadata: metadata, Media: media, MediaSize: mediaSize, MediaType: selected.MediaType, ChunkBytes: chunk}, nil
}

func uploadworkflowMode(value string) workflowcontract.UploadModeKind {
	return workflowcontract.UploadModeKind(value)
}

func uploadMedia(content *SelectedRequestRepresentation, params map[string]any) (io.Reader, int64, error) {
	key := content.PayloadParameter
	if key == "" {
		key = "media"
	}
	switch value := params[key].(type) {
	case []byte:
		return bytes.NewReader(value), int64(len(value)), nil
	case io.Reader:
		size, ok := numericInt64(params["media_size_bytes"])
		if !ok || size <= 0 {
			return nil, 0, errors.New("streaming upload requires media_size_bytes")
		}
		return value, size, nil
	case string:
		if selectedBinaryEncoding(content) == models.RequestBinaryEncodingBase64 {
			size := base64DecodedSize(value)
			return base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)), size, nil
		}
		return strings.NewReader(value), int64(len(value)), nil
	default:
		return nil, 0, errors.New("upload media is missing")
	}
}

func base64DecodedSize(value string) int64 {
	size := int64(base64.StdEncoding.DecodedLen(len(value)))
	if strings.HasSuffix(value, "==") {
		return size - 2
	}
	if strings.HasSuffix(value, "=") {
		return size - 1
	}
	return size
}

func numericInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		return int64(number), float64(int64(number)) == number
	default:
		return 0, false
	}
}

func (d *Dispatcher) uploadAttempt(srv *models.Service, obj *models.IntegrationObject, auths models.AuthConfigs, credentials map[string]any, client *http.Client) uploadworkflow.AttemptFunc {
	return func(ctx context.Context, req *http.Request, guard func(*url.URL) error) (*http.Response, error) {
		for key, value := range srv.DefaultHeaders {
			if req.Header.Get(key) == "" {
				req.Header.Set(key, value)
			}
		}
		// Workflow steps share the operation's reviewed response surface, so the
		// provider sees the same deterministic negotiation as ordinary dispatch.
		setProviderAcceptHeader(req, obj.Responses)
		if err := applySelectedAuthChecked(req, auths, credentials); err != nil {
			return nil, err
		}
		return d.executeUploadAttemptRetries(ctx, srv, obj, req, client, guard)
	}
}

func (d *Dispatcher) executeUploadAttemptRetries(ctx context.Context, srv *models.Service, obj *models.IntegrationObject, req *http.Request, client *http.Client, guard func(*url.URL) error) (*http.Response, error) {
	if srv.RetryConfig == nil {
		AddExecutionCount(ctx, "provider_attempt_count", 1)
		current, err := replayUploadRequest(req, 0)
		if err != nil {
			return nil, err
		}
		return d.doUploadAttempt(ctx, srv, obj, current, client, guard)
	}
	return d.executeUploadAttemptV3(ctx, srv, obj, req, client, guard)
}

func (d *Dispatcher) executeUploadAttemptV3(ctx context.Context, srv *models.Service, obj *models.IntegrationObject, req *http.Request, client *http.Client, guard func(*url.URL) error) (*http.Response, error) {
	var response *http.Response
	stepOperation := *obj
	// Upload workflows can change methods between steps, so retry policy must
	// classify the provider request being attempted rather than the outer operation.
	stepOperation.Method = req.Method
	_, err := d.executeRetryLoopV3(ctx, trace.SpanFromContext(ctx), srv, &stepOperation, NewBufferStream(), func(attemptCtx context.Context, attempt int, _ ResponseStream) (int, error) {
		if response != nil {
			_ = response.Body.Close()
		}
		current, replayErr := replayUploadRequest(req.WithContext(attemptCtx), attempt)
		if replayErr != nil {
			return 0, replayErr
		}
		recordRetryRequest(attemptCtx, current)
		response, replayErr = d.doUploadAttempt(attemptCtx, srv, obj, current, client, guard)
		if replayErr != nil {
			recordRetryTransportError(attemptCtx, replayErr)
		}
		if response != nil {
			recordRetryResponse(attemptCtx, response.Header)
			return response.StatusCode, replayErr
		}
		return 0, replayErr
	})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return nil, err
	}
	return response, nil
}

func replayUploadRequest(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 {
		return req, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("upload request body is not replayable")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, nil
}

func (d *Dispatcher) doUploadAttempt(ctx context.Context, srv *models.Service, obj *models.IntegrationObject, req *http.Request, client *http.Client, guard func(*url.URL) error) (*http.Response, error) {
	_, permit, err := d.awaitProviderRateLimitPermit(ctx, srv, obj)
	if err != nil {
		return nil, err
	}
	releasePending := true
	release := func() { d.releaseProviderRateLimit(context.WithoutCancel(ctx), permit) }
	defer func() {
		if releasePending {
			release()
		}
	}()
	scoped := *client
	scoped.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("upload redirect limit exceeded")
		}
		return guard(next.URL)
	}
	started := time.Now()
	response, err := scoped.Do(req)
	AddExecutionTiming(ctx, "provider_total", time.Since(started))
	if err != nil {
		return nil, err
	}
	bodyValues, err := captureQuotaSignalBody(srv.RateLimit, response)
	if err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	if err := d.syncProviderRateLimitResponse(ctx, srv, obj, response, bodyValues); err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	// Concurrency covers the provider response lifetime, not only time to
	// headers. Ownership moves to Body.Close so SSE and other streamed uploads
	// cannot free capacity while their response is still being consumed.
	response.Body = &uploadResponseBody{ReadCloser: response.Body, release: release}
	releasePending = false
	return response, nil
}

type uploadResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (body *uploadResponseBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(body.release)
	return err
}
