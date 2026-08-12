package workflowcontract_test

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/workflowcontract"
	"github.com/Usefused/engine/internal/testcontract"
)

// TestMediaUploadContractValidates keeps upload behavior executable without
// making a provider label part of the workflow contract.
func TestMediaUploadContractValidates(t *testing.T) {
	workflow := testcontract.UploadWorkflow()
	if err := workflowcontract.Validate(&workflow); err != nil {
		t.Fatal(err)
	}
	workflow.Modes[2].Steps[1].URL.AllowedOrigins = []string{"http://attacker.invalid"}
	if err := workflowcontract.Validate(&workflow); err == nil {
		t.Fatal("expected insecure upload origin rejection")
	}
}
