package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestSDKPackageRegenerationForwardsOriginalGenerationPin proves cache recovery sends refs through the existing generator rather than uploading contracts.
func TestSDKPackageRegenerationForwardsOriginalGenerationPin(t *testing.T) {
	appID := uuid.New()
	build := sdkPackageBuildRequest(appID)
	selection := build.Selections[0]
	pin := models.SDKContractBinding{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID, Version: "1.0", Revision: 7, SourceHash: "source-7", GenerationContractHash: "sha256:" + strings.Repeat("a", 64)}
	build.ContractBindings = []models.SDKContractBinding{pin}
	packages := &sdkPackageClientStub{responses: []*http.Response{sdkPackageResponse(http.StatusNotFound, "missing"), sdkPackageResponse(http.StatusOK, "zip")}}
	proxy := &sdkPackageGenerationForwarder{status: http.StatusAccepted, body: `{"app_id":"` + appID.String() + `","job_id":"retained-job","status":"complete","scope_schema_version":3,"generator_version":"registry-generator-v4"}`}
	response := serveSDKPackageDownload(t, appID, build, proxy, packages)
	// Recovery should produce a normal download after one generation request.
	if response.Code != http.StatusOK || len(packages.appIDs) != 2 {
		t.Fatalf("status=%d attempts=%d body=%s", response.Code, len(packages.appIDs), response.Body)
	}
	// The original reference must reach Registry unchanged even without live service reads.
	if len(proxy.request.ContractBindings) != 1 || proxy.request.ContractBindings[0] != pin {
		t.Fatalf("bindings=%+v", proxy.request.ContractBindings)
	}
}
