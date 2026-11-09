package executionappvm

import (
	"context"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// maxCallStackSize bounds recursion identically for authored apps, declaration inspection and trusted metadata.
const maxCallStackSize = 512

// newBoundedRuntime is the single place Engine and the execution worker construct a Goja interpreter.
// Every runtime gets the same stack ceiling, rejects external source maps, and is interrupted when ctx ends.
// The returned stop function must be called once the runtime is no longer in use.
func newBoundedRuntime(ctx context.Context, interruptReason string) (*goja.Runtime, func() bool) {
	vm := goja.New()
	// Inline-only maps apply to dynamically evaluated source as well as compiled bundles.
	vm.SetParserOptions(parser.WithSourceMapLoader(rejectExternalSourceMap))
	vm.SetMaxCallStackSize(maxCallStackSize)
	stop := context.AfterFunc(ctx, func() { vm.Interrupt(interruptReason) })
	return vm, stop
}
