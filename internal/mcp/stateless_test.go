package mcp

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The 2026-07-28 revision, served next to the initialize-based ones.
//
// Each rule the revision makes a MUST is asserted both ways where it can be: the
// request that satisfies it is served (a permit witness), and the request that
// breaks it in exactly one respect is refused with the code the spec names. The
// legacy half of the file asserts that the older clients see no change, since a
// dual-era server that breaks its existing callers to serve new ones has not
// added a revision, it has swapped one.

const statelessMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"stateless-probe","version":"1"}}`

type statelessCall struct {
	method  string
	params  string // JSON object body without the braces, _meta added unless noMeta
	noMeta  bool
	headers map[string]string
	skip    []string // standard headers to leave off
	id      string   // JSON id; empty means a notification
}

func (c statelessCall) do(t *testing.T, server *Server) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	parts := []string{}
	if c.params != "" {
		parts = append(parts, c.params)
	}
	if !c.noMeta {
		parts = append(parts, statelessMeta)
	}
	body := `{"jsonrpc":"2.0",`
	if c.id != "" {
		body += `"id":` + c.id + `,`
	}
	body += `"method":"` + c.method + `","params":{` + strings.Join(parts, ",") + `}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	std := map[string]string{"MCP-Protocol-Version": ProtocolStateless, "Mcp-Method": c.method}
	for _, name := range c.skip {
		delete(std, name)
	}
	for k, v := range std {
		req.Header.Set(k, v)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, decoded
}

func errorCode(t *testing.T, decoded map[string]any) int {
	t.Helper()
	e, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got %v", decoded)
	}
	return int(e["code"].(float64))
}

func resultOf(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	r, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got %v", decoded)
	}
	if r["resultType"] != "complete" {
		t.Fatalf("every 2026-07-28 result must say resultType complete, got %v", r["resultType"])
	}
	meta, _ := r["_meta"].(map[string]any)
	info, _ := meta[metaServerInfo].(map[string]any)
	if info["name"] != "kenwea-public-mcp" {
		t.Fatalf("result must name the server in _meta, got %v", r["_meta"])
	}
	return r
}

func assertCacheHints(t *testing.T, result map[string]any) {
	t.Helper()
	ttl, ok := result["ttlMs"].(float64)
	if !ok || ttl < 0 {
		t.Fatalf("a cacheable result must carry ttlMs >= 0, got %v", result["ttlMs"])
	}
	if scope := result["cacheScope"]; scope != "public" && scope != "private" {
		t.Fatalf("cacheScope must be public or private, got %v", scope)
	}
}

func TestStatelessDiscoverAndList(t *testing.T) {
	server := NewServer(StaticAuthenticator{})

	t.Run("server/discover names every revision, the tools capability and cache hints", func(t *testing.T) {
		rec, decoded := statelessCall{method: "server/discover", id: "1"}.do(t, server)
		if rec.Code != http.StatusOK {
			t.Fatalf("server/discover returned %d: %s", rec.Code, rec.Body.String())
		}
		result := resultOf(t, decoded)
		assertCacheHints(t, result)
		versions, _ := result["supportedVersions"].([]any)
		for _, want := range SupportedProtocolVersionList() {
			found := false
			for _, v := range versions {
				found = found || v == want
			}
			if !found {
				t.Fatalf("supportedVersions omits %s: %v", want, versions)
			}
		}
		caps, _ := result["capabilities"].(map[string]any)
		if _, ok := caps["tools"]; !ok || len(caps) != 1 {
			t.Fatalf("the server has tools and nothing else, got %v", caps)
		}
		if instructions, _ := result["instructions"].(string); !strings.Contains(instructions, "kenwea.sandbox.check") {
			t.Fatalf("instructions should point at the first useful call, got %q", instructions)
		}
		if rec.Header().Get("Mcp-Session-Id") != "" {
			t.Fatal("the stateless revision has no sessions; none may be minted")
		}
	})

	t.Run("tools/list is the same list the legacy era serves, with cache hints", func(t *testing.T) {
		rec, decoded := statelessCall{method: "tools/list", id: "2"}.do(t, server)
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/list returned %d: %s", rec.Code, rec.Body.String())
		}
		result := resultOf(t, decoded)
		assertCacheHints(t, result)
		tools, _ := result["tools"].([]any)
		if len(tools) != len(mcpToolDescriptors()) || len(tools) == 0 {
			t.Fatalf("expected %d tools, got %d", len(mcpToolDescriptors()), len(tools))
		}
	})
}

func TestStatelessToolCall(t *testing.T) {
	t.Run("an anonymous tools/call with matching headers is served and marked complete", func(t *testing.T) {
		server := NewServerWithRuntime(StaticAuthenticator{Err: http.ErrNoCookie}, nil, nil, registerForwarder{})
		rec, decoded := statelessCall{
			method:  "tools/call",
			id:      "3",
			params:  `"name":"kenwea.onboarding.registerSelf","arguments":{"agentName":"Stateless Agent"}`,
			headers: map[string]string{"Mcp-Name": "kenwea.onboarding.registerSelf"},
		}.do(t, server)
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/call returned %d: %s", rec.Code, rec.Body.String())
		}
		result := resultOf(t, decoded)
		structured, _ := result["structuredContent"].(map[string]any)
		if structured["touristMode"] != true {
			t.Fatalf("the tool result must come through unchanged, got %v", result)
		}
	})

	t.Run("an authenticated call mints no session, while the same call on the legacy era does", func(t *testing.T) {
		auth := StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_1", AgentID: "agent_1"}}
		rec, decoded := statelessCall{
			method:  "tools/call",
			id:      "4",
			params:  `"name":"kenwea.agent.sendHeartbeat","arguments":{}`,
			headers: map[string]string{"Mcp-Name": "kenwea.agent.sendHeartbeat", "Authorization": "Bearer kw_test"},
		}.do(t, NewServer(auth))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat returned %d: %s", rec.Code, rec.Body.String())
		}
		resultOf(t, decoded)
		if rec.Header().Get("Mcp-Session-Id") != "" {
			t.Fatal("a stateless request must not be answered with a session id")
		}

		// The metamorphic twin: identical call, legacy header, no _meta. It must
		// still get its session, or the legacy era has been changed by this one.
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"kenwea.agent.sendHeartbeat","arguments":{}}}`))
		req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
		req.Header.Set("Authorization", "Bearer kw_test")
		legacy := httptest.NewRecorder()
		NewServer(auth).ServeHTTP(legacy, req)
		if legacy.Code != http.StatusOK || legacy.Header().Get("Mcp-Session-Id") == "" {
			t.Fatalf("the legacy era must keep issuing sessions, got %d session=%q", legacy.Code, legacy.Header().Get("Mcp-Session-Id"))
		}
		if strings.Contains(legacy.Body.String(), "resultType") {
			t.Fatal("legacy results must stay exactly as they were, without resultType")
		}
	})

	t.Run("a stale session id is ignored rather than honoured or refused", func(t *testing.T) {
		// On the legacy era this exact header, with no Authorization, is a 401
		// invalid_session. On the stateless era sessions do not exist, so the
		// header is not evidence of anything and the request authenticates the
		// way every stateless request does.
		auth := StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_1", AgentID: "agent_1"}}
		rec, decoded := statelessCall{
			method:  "tools/call",
			id:      "6",
			params:  `"name":"kenwea.agent.sendHeartbeat","arguments":{}`,
			headers: map[string]string{"Mcp-Name": "kenwea.agent.sendHeartbeat", "Mcp-Session-Id": "sess_from_another_era"},
		}.do(t, NewServer(auth))
		if rec.Code != http.StatusOK {
			t.Fatalf("a stale session id must be ignored, got %d: %s", rec.Code, rec.Body.String())
		}
		resultOf(t, decoded)
	})

	t.Run("a base64-encoded Mcp-Name is decoded before it is compared", func(t *testing.T) {
		server := NewServerWithRuntime(StaticAuthenticator{Err: http.ErrNoCookie}, nil, nil, registerForwarder{})
		encoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("kenwea.onboarding.registerSelf")) + "?="
		rec, _ := statelessCall{
			method:  "tools/call",
			id:      "7",
			params:  `"name":"kenwea.onboarding.registerSelf","arguments":{}`,
			headers: map[string]string{"Mcp-Name": encoded},
		}.do(t, server)
		if rec.Code != http.StatusOK {
			t.Fatalf("an encoded header naming the same tool must be accepted, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestStatelessRefusals(t *testing.T) {
	server := NewServerWithRuntime(StaticAuthenticator{Err: http.ErrNoCookie}, nil, nil, registerForwarder{})
	call := `"name":"kenwea.onboarding.registerSelf","arguments":{}`

	cases := []struct {
		name   string
		call   statelessCall
		status int
		code   int
	}{
		{"missing _meta on a request that names 2026-07-28", statelessCall{method: "tools/list", id: "10", noMeta: true}, http.StatusBadRequest, codeInvalidParams},
		{"missing client capabilities", statelessCall{method: "tools/list", id: "11", noMeta: true, params: `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`}, http.StatusBadRequest, codeInvalidParams},
		{"a _meta version this server does not speak", statelessCall{method: "tools/list", id: "12", noMeta: true, params: `"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01","io.modelcontextprotocol/clientCapabilities":{}}`, headers: map[string]string{"MCP-Protocol-Version": "2099-01-01"}}, http.StatusBadRequest, codeUnsupportedProtocolVersion},
		{"no MCP-Protocol-Version header", statelessCall{method: "tools/list", id: "13", skip: []string{"MCP-Protocol-Version"}}, http.StatusBadRequest, codeHeaderMismatch},
		{"a 2026-07-28 header with _meta naming another version", statelessCall{method: "tools/list", id: "14", noMeta: true, params: `"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}`}, http.StatusBadRequest, codeUnsupportedProtocolVersion},
		{"no Mcp-Method header", statelessCall{method: "tools/list", id: "15", skip: []string{"Mcp-Method"}}, http.StatusBadRequest, codeHeaderMismatch},
		{"Mcp-Method disagrees with the body", statelessCall{method: "tools/list", id: "16", headers: map[string]string{"Mcp-Method": "server/discover"}}, http.StatusBadRequest, codeHeaderMismatch},
		{"tools/call without Mcp-Name", statelessCall{method: "tools/call", id: "17", params: call}, http.StatusBadRequest, codeHeaderMismatch},
		{"Mcp-Name names a different tool than the body", statelessCall{method: "tools/call", id: "18", params: call, headers: map[string]string{"Mcp-Name": "kenwea.marketplace.purchase"}}, http.StatusBadRequest, codeHeaderMismatch},
		{"ping is gone in this revision", statelessCall{method: "ping", id: "19"}, http.StatusNotFound, -32601},
		{"resources/list is a capability this server lacks", statelessCall{method: "resources/list", id: "20"}, http.StatusNotFound, -32601},
		{"subscriptions/listen is not offered", statelessCall{method: "subscriptions/listen", id: "21"}, http.StatusNotFound, -32601},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, decoded := tc.call.do(t, server)
			if rec.Code != tc.status {
				t.Fatalf("expected HTTP %d, got %d: %s", tc.status, rec.Code, rec.Body.String())
			}
			if got := errorCode(t, decoded); got != tc.code {
				t.Fatalf("expected JSON-RPC code %d, got %d: %s", tc.code, got, rec.Body.String())
			}
		})
	}

	t.Run("an unsupported version names what is supported and what was asked", func(t *testing.T) {
		rec, decoded := cases[2].call.do(t, server)
		data, _ := decoded["error"].(map[string]any)["data"].(map[string]any)
		if data["requested"] != "2099-01-01" {
			t.Fatalf("the refusal must echo the requested version, got %s", rec.Body.String())
		}
		supported, _ := data["supported"].([]any)
		if len(supported) != len(SupportedProtocolVersionList()) {
			t.Fatalf("the refusal must list every supported version, got %v", supported)
		}
	})

	t.Run("a notification is accepted with 202 and no body", func(t *testing.T) {
		rec, _ := statelessCall{method: "notifications/cancelled"}.do(t, server)
		if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
			t.Fatalf("expected 202 with no body, got %d %q", rec.Code, rec.Body.String())
		}
	})

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method+" naming 2026-07-28 is 405, since the revision is POST only", func(t *testing.T) {
			req := httptest.NewRequest(method, "/mcp/v1", nil)
			req.Header.Set("MCP-Protocol-Version", ProtocolStateless)
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("expected 405 with Allow: POST, got %d allow=%q", rec.Code, rec.Header().Get("Allow"))
			}
		})
	}
}

func TestLegacyEraIsUnchanged(t *testing.T) {
	server := NewServer(StaticAuthenticator{})

	t.Run("initialize still selects the legacy era, even under a 2026-07-28 header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`))
		req.Header.Set("MCP-Protocol-Version", ProtocolStateless)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"protocolVersion":"2025-11-25"`) {
			t.Fatalf("initialize must be answered the legacy way, got %d %s", rec.Code, rec.Body.String())
		}
	})

	// The initialize-based revisions require the server to answer with the
	// version the client asked for when it speaks it. It used to always say
	// 2025-11-25.
	for _, asked := range []string{ProtocolCurrent, ProtocolIntermediate, ProtocolCompat} {
		t.Run("initialize asking for "+asked+" is answered with "+asked, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+asked+`","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`))
			req.Header.Set("MCP-Protocol-Version", asked)
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), `"protocolVersion":"`+asked+`"`) {
				t.Fatalf("expected the server to agree to %s, got %s", asked, rec.Body.String())
			}
		})
	}

	t.Run("initialize asking for a version this server does not speak gets the newest legacy one", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`))
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), `"protocolVersion":"`+ProtocolCurrent+`"`) {
			t.Fatalf("expected %s, got %s", ProtocolCurrent, rec.Body.String())
		}
	})

	t.Run("a legacy unknown method is still 200 with -32601", func(t *testing.T) {
		rec := standardMethodRequest(t, "resources/list")
		if rec.Code != http.StatusOK {
			t.Fatalf("the legacy era answers an absent capability over a successful transport, got %d", rec.Code)
		}
	})

	t.Run("a legacy ping is still answered", func(t *testing.T) {
		rec := standardMethodRequest(t, "ping")
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("legacy ping must succeed, got %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a legacy GET still opens the ready stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/mcp/v1", nil)
		req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("legacy GET must keep working, got %d", rec.Code)
		}
	})

	t.Run("a legacy header with modern _meta is served the legacy way, as the published bridge sends it", func(t *testing.T) {
		// @kenwea/mcp 0.2.5 stamps its configured legacy version on every request,
		// so a modern client behind it arrives exactly like this. It was served
		// before the stateless era existed here and must still be.
		rec, decoded := statelessCall{
			method:  "tools/list",
			id:      "30",
			headers: map[string]string{"MCP-Protocol-Version": ProtocolCurrent},
			skip:    []string{"Mcp-Method"},
		}.do(t, server)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected the legacy answer, got %d %s", rec.Code, rec.Body.String())
		}
		result, _ := decoded["result"].(map[string]any)
		if tools, _ := result["tools"].([]any); len(tools) == 0 || result["resultType"] != nil {
			t.Fatalf("expected an unchanged legacy tools/list, got %s", rec.Body.String())
		}
	})

	t.Run("an unknown legacy header version uses the shape a dual-era client recognises", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("MCP-Protocol-Version", "1999-01-01")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		var decoded map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		if rec.Code != http.StatusBadRequest || errorCode(t, decoded) != codeUnsupportedProtocolVersion {
			t.Fatalf("expected 400 with %d, got %d %s", codeUnsupportedProtocolVersion, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), ProtocolStateless) {
			t.Fatalf("the refusal must name the new revision too, got %s", rec.Body.String())
		}
	})
}

func TestWithoutMetaKeepsEverythingElse(t *testing.T) {
	out := withoutMeta(json.RawMessage(`{"query":"lint","_meta":{"x":1},"limit":5}`))
	var fields map[string]any
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["_meta"]; ok || fields["query"] != "lint" || fields["limit"] != float64(5) {
		t.Fatalf("expected only _meta removed, got %s", out)
	}
	if string(withoutMeta(json.RawMessage(`{"a":1}`))) != `{"a":1}` {
		t.Fatal("params without _meta must be returned untouched")
	}
}
