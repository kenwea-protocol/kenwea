package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The base MCP method set, as a conforming client actually exercises it.
//
// Measured on production 2026-07-31: after the protocol-version fix removed 100% of
// the version rejections, 14 of the day's remaining 400s were these methods -- the
// ones a client calls immediately after initialize. The server answered HTTP 400,
// which says the transport failed, so a caller doing the standard thing concluded
// the server was broken rather than that a capability was absent.

func standardMethodRequest(t *testing.T, method string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":7,"method":"` + method + `"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(body))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	rec := httptest.NewRecorder()
	NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_1", AgentID: "agent_1"}}).ServeHTTP(rec, req)
	return rec
}

// ping is base protocol, not a capability. It must succeed.
func TestPingIsAnswered(t *testing.T) {
	rec := standardMethodRequest(t, "ping")
	if rec.Code != http.StatusOK {
		t.Fatalf("ping returned %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Result *map[string]any `json:"result"`
		Error  *struct{}       `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("ping response does not parse: %v", err)
	}
	if response.Error != nil || response.Result == nil {
		t.Fatalf("ping must return a result, got %s", rec.Body.String())
	}
}

// The capabilities we do not implement are refused with the standard code, over a
// successful HTTP transaction. Both halves matter and each is asserted separately:
// the HTTP status is what tells a client whether the server is reachable at all, and
// -32601 is the code clients special-case to mean "capability absent".
func TestUnimplementedCapabilitiesReturnMethodNotFoundNotTransportFailure(t *testing.T) {
	for _, method := range []string{
		"resources/list", "resources/templates/list", "resources/read",
		"prompts/list", "prompts/get", "completion/complete", "logging/setLevel",
	} {
		t.Run(method, func(t *testing.T) {
			rec := standardMethodRequest(t, method)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s returned HTTP %d; a well-formed request for an absent capability is not a transport failure", method, rec.Code)
			}
			var response struct {
				Result any `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("%s response does not parse: %v", method, err)
			}
			if response.Error == nil {
				t.Fatalf("%s returned a result; this server does not implement it", method)
			}
			if response.Error.Code != -32601 {
				t.Fatalf("%s returned JSON-RPC code %d, want -32601 (method not found)", method, response.Error.Code)
			}
			// Not an empty list. An empty `resources` array would claim the
			// capability exists and happens to hold nothing, which is not true --
			// initialize advertises `tools` and nothing else, and this answer has
			// to agree with that.
			if response.Result != nil {
				t.Fatalf("%s returned a result payload alongside the error: %s", method, rec.Body.String())
			}
		})
	}
}

// The answer must stay consistent with what initialize advertises. If a capability is
// ever added to that block, its methods must stop returning method-not-found -- and
// this test is what notices.
func TestRefusedCapabilitiesAreAbsentFromTheInitializeBlock(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	rec := httptest.NewRecorder()
	NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_1"}}).ServeHTTP(rec, req)

	var response struct {
		Result struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("initialize response does not parse: %v", err)
	}
	for _, capability := range []string{"resources", "prompts", "completions", "logging"} {
		if _, declared := response.Result.Capabilities[capability]; declared {
			t.Fatalf("initialize advertises %q but its methods return method-not-found; the two must agree", capability)
		}
	}
	if _, declared := response.Result.Capabilities["tools"]; !declared {
		t.Fatal("initialize no longer advertises tools, which is the one capability this server has")
	}
}

// notifications/initialized carries no id and expects no body. It was already
// handled; asserted here so the standard-method set is covered in one place.
func TestInitializedNotificationIsAccepted(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	rec := httptest.NewRecorder()
	NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_1"}}).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("notifications/initialized returned %d, want 202: %s", rec.Code, rec.Body.String())
	}
}
