package sandbox

import (
	"bytes"
	"errors"
	"testing"
)

// TestBoundedJSONResponseCollectorAcceptsOneSuccessfulDocument protects the rule that attacker-controlled documents and responses cannot exceed admitted work budgets.
func TestBoundedJSONResponseCollectorAcceptsOneSuccessfulDocument(t *testing.T) {
	collector := newBoundedJSONResponseCollector()
	if err := collector.SendResponseContract(200, "json"); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range [][]byte{[]byte(`{"items":`), []byte(`[1,2]}`)} {
		if err := collector.Send(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := collector.SendStatus(200); err != nil {
		t.Fatal(err)
	}
	result, err := collector.Result()
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Body) != `{"items":[1,2]}` {
		t.Fatalf("unexpected physical result: %#v", result)
	}
}

// TestBoundedJSONResponseCollectorRejectsOversizeBeforeBuffering protects the rule that attacker-controlled documents and responses cannot exceed admitted work budgets.
func TestBoundedJSONResponseCollectorRejectsOversizeBeforeBuffering(t *testing.T) {
	collector := newBoundedJSONResponseCollector()
	_ = collector.SendResponseContract(200, "json")
	err := collector.Send(bytes.Repeat([]byte("x"), maxPhysicalJSONResponseBytes+1))
	if !errors.Is(err, ErrPhysicalResponseTooLarge) {
		t.Fatalf("Send() error = %v, want ErrPhysicalResponseTooLarge", err)
	}
	if collector.body.Len() != 0 {
		t.Fatalf("oversize body retained %d bytes", collector.body.Len())
	}
}

// TestBoundedJSONResponseCollectorRejectsMalformedOrUnsuccessfulResponses protects the rule that attacker-controlled documents and responses cannot exceed admitted work budgets.
func TestBoundedJSONResponseCollectorRejectsMalformedOrUnsuccessfulResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		family string
		body   string
		want   error
	}{
		{name: "trailing document", status: 200, family: "json", body: `{}` + `{}`, want: ErrPhysicalResponseNotJSON},
		{name: "duplicate object key", status: 200, family: "json", body: `{"id":1,"id":2}`, want: ErrPhysicalResponseNotJSON},
		{name: "non json media", status: 200, family: "text", body: `{}`, want: ErrPhysicalResponseNotJSON},
		{name: "provider failure", status: 429, family: "json", body: `{}`, want: ErrPhysicalResponseStatus},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := newBoundedJSONResponseCollector()
			_ = collector.SendResponseContract(test.status, test.family)
			_ = collector.Send([]byte(test.body))
			_, err := collector.Result()
			if !errors.Is(err, test.want) {
				t.Fatalf("Result() error = %v, want %v", err, test.want)
			}
		})
	}
}
