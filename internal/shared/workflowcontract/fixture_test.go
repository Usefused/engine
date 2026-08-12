package workflowcontract_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Usefused/engine/internal/shared/workflowcontract"
)

// TestMediaUploadFixtureValidates keeps upload behavior executable without
// making a provider label part of the workflow contract.
func TestMediaUploadFixtureValidates(t *testing.T) {
	body, err := os.ReadFile("../../../../contract-fixtures/workflow/v1_media_upload.json")
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowcontract.UploadWorkflow
	if err := json.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	if err := workflowcontract.Validate(&workflow); err != nil {
		t.Fatal(err)
	}
	workflow.Modes[2].Steps[1].URL.AllowedOrigins = []string{"http://attacker.invalid"}
	if err := workflowcontract.Validate(&workflow); err == nil {
		t.Fatal("expected insecure upload origin rejection")
	}
}
