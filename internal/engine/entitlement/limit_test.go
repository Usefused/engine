package entitlement

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/trace"
)

func noopSpan() trace.Span {
	_, span := trace.NewNoopTracerProvider().Tracer("").Start(context.Background(), "test")
	return span
}

func TestCheckLimit_Unlimited(t *testing.T) {
	if err := CheckLimit(noopSpan(), "buckets", 1000, models.IntPtr(-1)); err != nil {
		t.Fatalf("limit -1 should allow, got %v", err)
	}
}

func TestCheckLimit_ZeroMeansDisallowed(t *testing.T) {
	if err := CheckLimit(noopSpan(), "buckets", 0, models.IntPtr(0)); err == nil {
		t.Fatal("limit 0 should block")
	}
	if err := CheckLimit(noopSpan(), "buckets", 5, models.IntPtr(0)); err == nil {
		t.Fatal("limit 0 should block even when current > 0")
	}
}

func TestCheckLimit_HardCeiling(t *testing.T) {
	// Below limit: allow
	if err := CheckLimit(noopSpan(), "buckets", 2, models.IntPtr(5)); err != nil {
		t.Fatalf("2/5 should allow, got %v", err)
	}
	// At limit: block (adding one more would exceed)
	if err := CheckLimit(noopSpan(), "buckets", 5, models.IntPtr(5)); err == nil {
		t.Fatal("5/5 should block")
	}
	// Over limit: block
	if err := CheckLimit(noopSpan(), "buckets", 6, models.IntPtr(5)); err == nil {
		t.Fatal("6/5 should block")
	}
}

func TestCheckLimit_ErrorMessage(t *testing.T) {
	errZero := CheckLimit(noopSpan(), "families", 0, models.IntPtr(0))
	if errZero.Error() != "families creation not allowed" {
		t.Fatalf("unexpected zero-limit message: %q", errZero.Error())
	}
	errCeil := CheckLimit(noopSpan(), "families", 3, models.IntPtr(3))
	if errCeil.Error() != "families limit reached (3/3)" {
		t.Fatalf("unexpected ceiling message: %q", errCeil.Error())
	}
}
