package platformapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A forwarded tool must not inherit the authentication budget.
//
// It did, and it cost the sandbox check every artifact that took more than five
// seconds: fetch, scan, hand to the runner, docker run. Small files finished and
// large ones did not, so the tool looked size-limited when it was time-limited,
// and the caller was told "platform api is temporarily unavailable" -- our
// budget, reported as our infrastructure failing.
func TestForwardedToolGetsALongerBudgetThanAuthentication(t *testing.T) {
	a := New("http://example.invalid")
	if a.Client.Timeout != authTimeout {
		t.Fatalf("auth client should keep the short budget, got %s", a.Client.Timeout)
	}
	if a.ToolClient.Timeout <= a.Client.Timeout {
		t.Fatalf("a forwarded tool must get more time than an identify lookup: tool=%s auth=%s",
			a.ToolClient.Timeout, a.Client.Timeout)
	}
	// The sandbox is the slowest thing behind this: 45s for a package plus the
	// runner client's extra 10s. A budget under that fires in the wrong place
	// and reports the wrong cause.
	if a.ToolClient.Timeout < 60*time.Second {
		t.Fatalf("tool budget %s is below the sandbox package budget it has to cover", a.ToolClient.Timeout)
	}
}

// The permit witness: a slow-but-successful tool call must actually come back,
// not merely be allowed longer in a config struct.
func TestASlowForwardedToolStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"verdict": "approved"}})
	}))
	defer srv.Close()

	a := New(srv.URL)
	// Squeeze auth's budget below the handler's delay: if ForwardTool ever goes
	// back to using it, this test fails instead of passing quietly.
	a.Client = &http.Client{Timeout: 300 * time.Millisecond}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	out, err := a.ForwardTool(req, "kenwea.sandbox.check", json.RawMessage(`{"artifactRef":"https://example.com/x.js"}`))
	if err != nil {
		t.Fatalf("a slow tool call must succeed on the tool budget, got: %v", err)
	}
	if out == nil {
		t.Fatal("expected a payload back")
	}
}
