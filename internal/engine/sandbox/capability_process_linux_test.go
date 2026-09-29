//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/executionappvm"
)

// TestInspectCapabilityBundleInIsolatedWorker exercises a large authored bundle in the real Linux child.
func TestInspectCapabilityBundleInIsolatedWorker(t *testing.T) {
	const declaration = `globalThis.FusedExecutionManifest={schemaVersion:1,inputSchema:{type:"object"},outputSchema:{type:"object"},searchable:[],selectedOperations:[]};globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>({})};`
	// The large bundle catches worker limits that a tiny availability probe cannot reveal.
	bundle := []byte(declaration + strings.Repeat("/* bundled dependency padding */", 25000))
	manifest, err := InspectCapabilityBundle(context.Background(), bundle)
	// Only explicit OS rejection of namespace creation or chroot makes this integration test inapplicable.
	if errors.Is(err, ErrCapabilityWorkerUnavailable) && linuxCapabilityIsolationDenied(t) {
		t.Skip("Linux host policy denies worker isolation")
	}
	if err != nil {
		t.Fatalf("isolated bundle inspection: %v", err)
	}
	const want = `{"schemaVersion":1,"inputSchema":{"type":"object"},"outputSchema":{"type":"object"},"searchable":[],"selectedOperations":[]}`
	if string(manifest) != want {
		t.Fatalf("isolated manifest = %s", manifest)
	}
}

// TestRunCapabilityScriptInIsolatedWorker verifies provider and data IPC with the packaged child.
func TestRunCapabilityScriptInIsolatedWorker(t *testing.T) {
	// A denied isolation boundary is environmental; a crash or failed resource cap must still fail.
	if !IsCapabilityWorkerAvailable(context.Background()) && linuxCapabilityIsolationDenied(t) {
		t.Skip("Linux host policy denies worker isolation")
	}
	const bundle = `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},async execute({input}){const found=JSON.parse(await __fusedHost.fetch(JSON.stringify({input:{value:input.name}})));await __fusedHost.dbSet(JSON.stringify({id:found.id}));return {id:found.id}}};`
	host := &capabilityScriptTestHost{}
	output, err := RunCapabilityScript(context.Background(), []byte(bundle), json.RawMessage(`{"name":"Jane"}`), host)
	// The isolated child must complete the same JSON-only provider and storage round trip as a live execution.
	if err != nil || string(output) != `{"id":"Jane"}` || string(host.data) != `{"id":"Jane"}` {
		t.Fatalf("isolated output=%s data=%s error=%v", output, host.data, err)
	}
}

// linuxCapabilityIsolationDenied checks the complete isolation bootstrap without evaluating any authored code.
func linuxCapabilityIsolationDenied(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command, cleanup, err := capabilityWorkerCommand(ctx)
	// Command setup failure is a worker regression, so the caller must fail its original assertion.
	if err != nil {
		return false
	}
	defer cleanup()
	// Empty stdin exits immediately after confinement and resource limits; captured output contains no authored source.
	output, err := command.CombinedOutput()
	// A namespace rejected before exec is distinct from a child that started and crashed.
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOSYS) {
		return true
	}
	var exit *exec.ExitError
	// Only the reserved pre-evaluation policy-denial exit may skip; generic exit 1, signals, and timeouts cannot.
	if errors.As(err, &exit) && exit.ExitCode() == executionappvm.WorkerIsolationDeniedExitCode {
		return true
	}
	// Keep unexpected bootstrap failures visible in CI without logging user bundles or invocation data.
	t.Logf("worker isolation probe: %v; stderr/stdout: %.4096s", err, output)
	return false
}
