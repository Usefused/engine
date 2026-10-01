package api

import (
	"context"
	"errors"
	"io"
	"os"
)

// Compilation includes an inspector child; reject excess work rather than accumulating a memory-heavy queue.
var executionCompileSlots = make(chan struct{}, 2)

// admitExecutionCompile bounds aggregate build concurrency across all accounts on this Engine process.
func admitExecutionCompile(ctx context.Context) (func(), error) {
	// A disconnected caller must not consume capacity even when a slot is free.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case executionCompileSlots <- struct{}{}:
		return func() { <-executionCompileSlots }, nil
	default:
		// Backpressure is a retryable deployment condition, never a TypeScript validation error.
		return nil, workspaceConfigHTTPError{status: 503, message: "unified app compiler is busy; retry later"}
	}
}

type executionCompilerOutput struct{ bytes []byte }

// Write drains child diagnostics while retaining only a bounded prefix in the Engine process.
func (output *executionCompilerOutput) Write(data []byte) (int, error) {
	remaining := (64 << 10) - len(output.bytes)
	// Continuing to drain avoids blocking the child on a full pipe after the diagnostic limit.
	if remaining > len(data) {
		remaining = len(data)
	}
	output.bytes = append(output.bytes, data[:remaining]...)
	return len(data), nil
}

// readBoundedCompilerFile enforces allocation bounds before reading attacker-influenced compiler artifacts.
func readBoundedCompilerFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	// A missing output must not be replaced with stale or caller-supplied artifacts.
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	// Read one sentinel byte beyond the cap to distinguish oversized files without loading them in full.
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("invalid compiler artifact size")
	}
	return data, nil
}
