package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
)

// requireCompileIsolation requires the production process isolation boundary before integration checks.
func requireCompileIsolation(t *testing.T) {
	t.Helper()
	// OS-constrained developer hosts cannot run this integration; Linux CI must exercise it with isolation enabled.
	if !sandbox.IsCapabilityWorkerAvailable(context.Background()) {
		t.Skip("isolated execution worker is unavailable on this host")
	}
}

// TestExecutionCompileAdmission applies backpressure before concurrent plans can allocate more child processes.
func TestExecutionCompileAdmission(t *testing.T) {
	first, err := admitExecutionCompile(context.Background())
	// Capacity must be available before this nonparallel test reserves it.
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := admitExecutionCompile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = admitExecutionCompile(context.Background())
	var busy workspaceConfigHTTPError
	// Exhaustion must be retryable and must not enqueue another memory-heavy compiler.
	if !errors.As(err, &busy) || busy.status != 503 {
		t.Fatalf("expected backpressure, got %v", err)
	}
	second()
	reused, err := admitExecutionCompile(context.Background())
	// Completion must return capacity to subsequent plans.
	if err != nil {
		t.Fatal(err)
	}
	reused()
}

// TestExecutionCompilerOutputAndFileBounds prevents child output from allocating unbounded Engine memory.
func TestExecutionCompilerOutputAndFileBounds(t *testing.T) {
	output := &executionCompilerOutput{}
	raw := []byte(strings.Repeat("x", 128<<10))
	n, err := output.Write(raw)
	// Discarded diagnostics still count as consumed so the child cannot block on its pipe.
	if err != nil || n != len(raw) || len(output.bytes) != 64<<10 {
		t.Fatal("diagnostic cap failed")
	}
	_, _ = output.Write(raw)
	if len(output.bytes) != 64<<10 {
		t.Fatal("repeated writes bypassed the cap")
	}
	file := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedCompilerFile(file, 1024); err == nil {
		t.Fatal("oversized artifact admitted")
	}
}

// TestExecutionCompilerTopLevelIsolation exercises the complete compiler and confined inspection path.
func TestExecutionCompilerTopLevelIsolation(t *testing.T) {
	requireCompileIsolation(t)
	t.Setenv("FUSED_EXECUTION_COMPILER", "../../../runtime/execution/dist/src/cli.js")
	pins := []executionCompilerSelection{{Service: "greeting", Operation: "greet", ServiceID: uuid.NewString(), ServiceVersionID: uuid.NewString(), EndpointID: uuid.NewString()}}
	base := `import {z} from "zod"; import {buildUnifiedApp} from "@fused/unified-app";
export default buildUnifiedApp({input:z.object({}),output:z.object({}),async execute(){return {}}});`
	probe := `const world = globalThis as any;
if (typeof world.process !== "undefined" || typeof world.require !== "undefined" || typeof world.__fusedHost !== "undefined") throw new Error("ambient authority leaked");
if (world.constructor.constructor("return typeof process")() !== "undefined") throw new Error("Node escape");
`
	artifact, err := runExecutionCompiler(context.Background(), probe+base, pins)
	// Top-level code may declare an app but must have neither Engine effects nor Node ambient authority.
	if err != nil || artifact == nil {
		t.Fatalf("confined declaration failed: %v", err)
	}
	artifact, err = runExecutionCompiler(context.Background(), `throw new Error("private authored failure");`+base, pins)
	// Invalid declarations must not produce deployable artifacts or leak authored exception text.
	if err == nil || artifact != nil || strings.Contains(err.Error(), "private authored failure") {
		t.Fatalf("invalid declaration result: %v", err)
	}
}
