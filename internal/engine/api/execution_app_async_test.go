package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCapabilityWaitBudget keeps caller wait preferences bounded and order independent.
func TestCapabilityWaitBudget(t *testing.T) {
	cases := []struct {
		prefer string
		wait   time.Duration
		async  bool
		bad    bool
	}{
		{prefer: "", wait: 0, async: false},
		{prefer: "respond-async", wait: 0, async: true},
		{prefer: "respond-async, wait=3", wait: 3 * time.Second, async: true},
		{prefer: "wait=3, respond-async", wait: 3 * time.Second, async: true},
		{prefer: "wait=31", bad: true},
	}
	for _, item := range cases {
		request := httptest.NewRequest(http.MethodPost, "/", nil)
		// An absent header keeps the existing synchronous behavior.
		if item.prefer != "" {
			request.Header.Set("Prefer", item.prefer)
		}
		wait, async, err := capabilityWaitBudget(request)
		// Invalid budgets cannot start work whose response timing the caller did not request.
		if (err != nil) != item.bad || (!item.bad && (wait != item.wait || async != item.async)) {
			t.Fatalf("Prefer %q = (%s, %t, %v)", item.prefer, wait, async, err)
		}
	}
}
