package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type sdkPackageBuildStoreStub struct {
	store.Store
	request *models.SDKGenerationRequest
	err     error
}

func (s *sdkPackageBuildStoreStub) GetSDKPackageBuildRequest(_ context.Context, _, _ uuid.UUID) (*models.SDKGenerationRequest, error) {
	return s.request, s.err
}

type sdkPackageClientStub struct {
	responses []*http.Response
	appIDs    []uuid.UUID
}

func (c *sdkPackageClientStub) DownloadSDKPackage(_ context.Context, appID uuid.UUID) (*http.Response, error) {
	c.appIDs = append(c.appIDs, appID)
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response, nil
}

type sdkPackageGenerationForwarder struct {
	status  int
	body    string
	request models.SDKGenerationRequest
}

func (f *sdkPackageGenerationForwarder) Forward(w http.ResponseWriter, r *http.Request, _ string) {
	_ = json.NewDecoder(r.Body).Decode(&f.request)
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.body))
}

// ForwardAndInspect is intentionally inert because package-generation tests
// exercise only the ordinary forwarding method.
func (f *sdkPackageGenerationForwarder) ForwardAndInspect(http.ResponseWriter, *http.Request, string, func(*http.Response, []byte)) {
}

func TestSDKPackageDownloadStreamsCacheHitWithoutGeneration(t *testing.T) {
	appID := uuid.New()
	build := sdkPackageBuildRequest(appID)
	packages := &sdkPackageClientStub{responses: []*http.Response{sdkPackageResponse(http.StatusOK, "zip-bytes")}}
	proxy := &sdkPackageGenerationForwarder{}
	recorder := serveSDKPackageDownload(t, appID, build, proxy, packages)

	if recorder.Code != http.StatusOK || recorder.Body.String() != "zip-bytes" {
		t.Fatalf("download response = %d %q", recorder.Code, recorder.Body.String())
	}
	if proxy.request.AppID != uuid.Nil {
		t.Fatal("cache hit unexpectedly regenerated the SDK")
	}
}

func TestSDKPackageDownloadRegeneratesPinnedCacheMissOnce(t *testing.T) {
	appID := uuid.New()
	build := sdkPackageBuildRequest(appID)
	packages := &sdkPackageClientStub{responses: []*http.Response{
		sdkPackageResponse(http.StatusNotFound, `{"error":"SDK package not found"}`),
		sdkPackageResponse(http.StatusOK, "regenerated-zip"),
	}}
	proxy := &sdkPackageGenerationForwarder{status: http.StatusAccepted, body: `{"app_id":"` + appID.String() + `","job_id":"job-1","status":"complete","scope_schema_version":3,"generator_version":"registry-generator-v1"}`}
	recorder := serveSDKPackageDownload(t, appID, build, proxy, packages)

	if recorder.Code != http.StatusOK || recorder.Body.String() != "regenerated-zip" {
		t.Fatalf("regenerated download = %d %q", recorder.Code, recorder.Body.String())
	}
	if len(packages.appIDs) != 2 {
		t.Fatalf("package attempts = %d, want 2", len(packages.appIDs))
	}
	if proxy.request.AppID != appID || proxy.request.AppFamilyID != build.AppFamilyID ||
		proxy.request.SourceHash != build.SourceHash || proxy.request.GeneratorVersion != build.GeneratorVersion ||
		proxy.request.IdempotencyKey != build.IdempotencyKey {
		t.Fatalf("regeneration changed immutable request: %#v", proxy.request)
	}
}

func TestSDKPackageDownloadReportsPinnedGeneratorUnavailable(t *testing.T) {
	appID := uuid.New()
	packages := &sdkPackageClientStub{responses: []*http.Response{sdkPackageResponse(http.StatusNotFound, "missing")}}
	proxy := &sdkPackageGenerationForwarder{status: http.StatusConflict, body: `{"error":"sdk_generator_version_unavailable"}`}
	recorder := serveSDKPackageDownload(t, appID, sdkPackageBuildRequest(appID), proxy, packages)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("response status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response workspaceConfigErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error.Code != "sdk_generator_version_unavailable" {
		t.Fatalf("error code = %q", response.Error.Code)
	}
}

func serveSDKPackageDownload(t *testing.T, appID uuid.UUID, build *models.SDKGenerationRequest, proxy Forwarder, packages SDKPackageClient) *httptest.ResponseRecorder {
	t.Helper()
	accountID := uuid.New()
	router := chi.NewRouter()
	router.Get("/sdks/{app_id}/download", SDKPackageDownloadHandler(&sdkPackageBuildStoreStub{request: build}, proxy, packages))
	request := httptest.NewRequest(http.MethodGet, "/sdks/"+appID.String()+"/download", nil)
	request.Header.Set("X-API-Key", "user-key")
	actor := accesscontrol.Actor{AccountID: accountID, SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser}
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func sdkPackageBuildRequest(appID uuid.UUID) *models.SDKGenerationRequest {
	return &models.SDKGenerationRequest{
		Name: "jira-sdk", Version: "2.0.0", AppFamilyID: uuid.New(), AppID: appID,
		SourceHash:       "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GeneratorVersion: models.SDKGeneratorVersion, IdempotencyKey: uuid.NewString(),
		TargetType: "sdk", TargetLanguage: "typescript",
		Selections: []models.SDKSelection{{ServiceID: uuid.New(), ServiceVersionID: uuid.New()}},
	}
}

func sdkPackageResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Body: io.NopCloser(bytes.NewBufferString(body)),
		Header: http.Header{"Content-Type": []string{"application/zip"}},
	}
}
