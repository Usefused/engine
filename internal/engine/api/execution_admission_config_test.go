package api

import (
	"github.com/Usefused/engine/internal/shared/config"
	"testing"
)

// TestConfigureUnifiedAppAdmissionBeforeUse ensures startup passes one immutable policy to the shared executor.
func TestConfigureUnifiedAppAdmissionBeforeUse(t *testing.T) {
	server := &EngineGRPCServer{}
	policy := config.UnifiedAppAdmissionConfig{MaxConcurrency: 2, QueueCapacity: 7, QueueTimeoutSeconds: 3}
	// A validated startup policy is accepted before any lazy initialization.
	if err := server.ConfigureUnifiedAppAdmission(policy); err != nil {
		t.Fatal(err)
	}
	manager := server.capabilityWorkerManager()
	defer manager.Close()
	// A live replacement would split counters and permit more work than the configured budget.
	if err := server.ConfigureUnifiedAppAdmission(policy); err == nil {
		t.Fatal("replaced active manager")
	}
	if manager != server.capabilityWorkerManager() {
		t.Fatal("configuration did not bind the shared manager")
	}
}

// TestInvalidUnifiedAppAdmissionDoesNotInitialize permits correcting a rejected policy before startup.
func TestInvalidUnifiedAppAdmissionDoesNotInitialize(t *testing.T) {
	server := &EngineGRPCServer{}
	// Invalid configuration cannot consume the executor's one-time initialization guard.
	if err := server.ConfigureUnifiedAppAdmission(config.UnifiedAppAdmissionConfig{}); err == nil {
		t.Fatal("accepted empty policy")
	}
	if err := server.ConfigureUnifiedAppAdmission(config.DefaultUnifiedAppAdmissionConfig()); err != nil {
		t.Fatal(err)
	}
	server.capabilityWorkerManager().Close()
}
