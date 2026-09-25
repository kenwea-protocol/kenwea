package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	NewServer(StaticAuthenticator{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// We are not an A2A agent, so no Agent Card. What is asserted: the status stays
// 404, so no A2A client can mistake us for a card, and the body names what we do
// speak and where to read it.
func TestAgentCardPathsPointToMCPWithoutClaimingA2A(t *testing.T) {
	for _, path := range []string{"/.well-known/agent-card.json", "/.well-known/agent.json"} {
		rec := get(t, path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s must stay 404, got %d", path, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s body is not JSON: %v", path, err)
		}
		if body["protocol"] != "mcp" || !strings.Contains(body["descriptor"].(string), "/.well-known/mcp") {
			t.Fatalf("%s must point to the MCP descriptor, got %v", path, body)
		}
		for _, cardField := range []string{"supportedInterfaces", "skills", "capabilities", "name"} {
			if _, ok := body[cardField]; ok {
				t.Fatalf("%s must not carry Agent Card field %q", path, cardField)
			}
		}
	}
}

func TestGlamaClaimIsServedOnlyWhenAValidTokenIsConfigured(t *testing.T) {
	t.Run("unset stays 404", func(t *testing.T) {
		t.Setenv("KENWEA_GLAMA_CLAIM", "")
		if rec := get(t, "/.well-known/glama.json"); rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
	t.Run("a malformed token is not served", func(t *testing.T) {
		t.Setenv("KENWEA_GLAMA_CLAIM", "glama_claim_tooshort")
		if rec := get(t, "/.well-known/glama.json"); rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
	t.Run("a valid token is served in Glama's schema", func(t *testing.T) {
		token := "glama_claim_" + strings.Repeat("A", 32)
		t.Setenv("KENWEA_GLAMA_CLAIM", token)
		rec := get(t, "/.well-known/glama.json")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["claim"] != token || body["$schema"] != "https://glama.ai/mcp/schemas/connector.json" {
			t.Fatalf("unexpected body %v", body)
		}
	})
}
