package platformapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// kenwea.notary.check goes to the keyless route when the caller sent no key and
// to the keyed one when it did; and the caller's address travels with the
// internal token so the platform counts callers, not this server.
func TestNotaryCheckRoutesByKeyAndForwardsTheCallerAddress(t *testing.T) {
	type seen struct{ path, clientIP, token, auth string }
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{r.URL.Path, r.Header.Get("X-Kenwea-Client-IP"), r.Header.Get("X-Kenwea-Forward-Token"), r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"checked": true}})
	}))
	defer srv.Close()
	a := New(srv.URL)
	params := json.RawMessage(`{"package":"express@4.18.2"}`)

	t.Setenv("KENWEA_INTERNAL_FORWARD_TOKEN", "internal-test-token")
	req := httptest.NewRequest(http.MethodPost, "/notary/v1", nil)
	req.Header.Set("X-Real-IP", "198.51.100.9")
	if _, err := a.ForwardTool(req, "kenwea.notary.check", params); err != nil {
		t.Fatalf("keyless forward: %v", err)
	}
	if got.path != "/public/sandbox/check" || got.clientIP != "198.51.100.9" || got.token != "internal-test-token" || got.auth != "" {
		t.Fatalf("keyless call forwarded as %+v", got)
	}

	req.Header.Set("Authorization", "Bearer kw_test")
	if _, err := a.ForwardTool(req, "kenwea.notary.check", params); err != nil {
		t.Fatalf("keyed forward: %v", err)
	}
	if got.path != "/agent/sandbox/check" || got.auth != "Bearer kw_test" {
		t.Fatalf("keyed call forwarded as %+v", got)
	}

	// No token configured: no address is claimed at all.
	t.Setenv("KENWEA_INTERNAL_FORWARD_TOKEN", "")
	if _, err := a.ForwardTool(req, "kenwea.sandbox.check", json.RawMessage(`{"artifactRef":"https://example.com/x.js"}`)); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got.clientIP != "" || got.token != "" {
		t.Fatalf("without a configured token nothing may be forwarded, got %+v", got)
	}
}
