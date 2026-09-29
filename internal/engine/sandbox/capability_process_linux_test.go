//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"
)

// TestInspectCapabilityBundleInIsolatedWorker exercises a large authored bundle in the real Linux child.
func TestInspectCapabilityBundleInIsolatedWorker(t *testing.T) {
	const declaration = `globalThis.FusedExecutionManifest={schemaVersion:1,inputSchema:{type:"object"},outputSchema:{type:"object"},searchable:[],selectedOperations:[]};globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>({})};`
	// The large bundle catches worker limits that a tiny availability probe cannot reveal.
	bundle := []byte(declaration + strings.Repeat("/* bundled dependency padding */", 25000))
	manifest, err := InspectCapabilityBundle(context.Background(), bundle)
	// Only an OS policy that blocks namespace creation can make this integration test inapplicable.
	if errors.Is(err, ErrCapabilityWorkerUnavailable) && linuxCapabilityNamespaceDenied(t) {
		t.Skip("Linux host policy denies worker namespaces")
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
	// A denied namespace is an environmental restriction; an already-started worker crash must fail.
	if !IsCapabilityWorkerAvailable(context.Background()) && linuxCapabilityNamespaceDenied(t) {
		t.Skip("Linux host policy denies worker namespaces")
	}
	const bundle = `globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},async execute({input}){const found=JSON.parse(await __fusedHost.fetch(JSON.stringify({input:{value:input.name}})));await __fusedHost.dbSet(JSON.stringify({id:found.id}));return {id:found.id}}};`
	host := &capabilityScriptTestHost{}
	output, err := RunCapabilityScript(context.Background(), []byte(bundle), json.RawMessage(`{"name":"Jane"}`), host)
	// The isolated child must complete the same JSON-only provider and storage round trip as a live execution.
	if err != nil || string(output) != `{"id":"Jane"}` || string(host.data) != `{"id":"Jane"}` {
		t.Fatalf("isolated output=%s data=%s error=%v", output, host.data, err)
	}
}

// linuxCapabilityNamespaceDenied distinguishes host policy from a worker crash after isolation.
func linuxCapabilityNamespaceDenied(t *testing.T) bool {
	t.Helper()
	command, cleanup, err := capabilityWorkerCommand(context.Background())
	// Command setup failure is a worker regression, so the caller must fail its original assertion.
	if err != nil {
		return false
	}
	defer cleanup()
	err = command.Start()
	// EPERM, EACCES, or ENOSYS indicates the host rejected the namespace boundary itself.
	if err != nil {
		return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOSYS)
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	return false
}
