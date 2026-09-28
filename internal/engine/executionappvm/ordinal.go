package executionappvm

import "context"

type callOrdinalKey struct{}

// CallOrdinal returns the interpreter-assigned host call sequence for child IPC.
func CallOrdinal(ctx context.Context) (int, bool) {
	ordinal, ok := ctx.Value(callOrdinalKey{}).(int)
	// Missing or forged sequence values cannot authorize an Engine host effect.
	if !ok || ordinal < 1 || ordinal > MaxHostCalls {
		return 0, false
	}
	return ordinal, true
}
