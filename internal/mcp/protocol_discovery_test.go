package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Measured on mcp.kenwea.com 2026-07-30: 54 of 104 requests to /mcp/v1 were rejected
// 400 against 29 that succeeded, and the callers were MCP directory crawlers --
// AgenstryBot, aisec-registry, mcpgrade-probe, 402explorer,
// agent-tools.cloud-crawler. Both causes were in the version check, and both were
// invisible to every test we had because our own bridge hardcodes two accepted
// strings: the check could only fail for a client we had not written.
func TestProtocolVersionHeaderIsToleratedAndNegotiated(t *testing.T) {
	server := NewServer(StaticAuthenticator{})
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`

	t.Run("an absent header is not a rejection", func(t *testing.T) {
		// A client's first request is initialize, at which point it has not negotiated
		// anything, so the header is legitimately missing. Refusing it meant every
		// spec-following first contact failed -- and a crawler is only ever a first
		// contact.
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(initialize))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "unsupported_protocol_version") {
			t.Fatalf("initialize with no MCP-Protocol-Version header was refused as an unsupported version: %s", rec.Body.String())
		}
	})

	t.Run("every advertised version is accepted", func(t *testing.T) {
		// The descriptor publishes this list. A version we advertise and refuse is a
		// worse failure than one we never mention, because the client did what the
		// document told it to.
		for _, version := range SupportedProtocolVersionList() {
			req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(initialize))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("MCP-Protocol-Version", version)
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "unsupported_protocol_version") {
				t.Fatalf("version %s is advertised by the descriptor and refused by the handler: %s", version, rec.Body.String())
			}
		}
	})

	t.Run("2025-06-18 specifically, since that is the one that was refused", func(t *testing.T) {
		if !supportedProtocolVersions[ProtocolIntermediate] {
			t.Fatalf("%s is a published MCP revision this server can serve; refusing it turns away correct clients", ProtocolIntermediate)
		}
	})

	t.Run("a genuinely unknown version is still refused, and says what we speak", func(t *testing.T) {
		// The tolerance above must not become "accept anything", or the header stops
		// carrying information.
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(initialize))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", "1999-01-01")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("an unknown protocol version was accepted, got %d", rec.Code)
		}
		for _, version := range SupportedProtocolVersionList() {
			if !strings.Contains(rec.Body.String(), version) {
				t.Fatalf("the refusal does not name %s, so the caller cannot tell what to send instead: %s", version, rec.Body.String())
			}
		}
	})
}

// The same access log showed crawlers requesting /.well-known/mcp and robots.txt and
// receiving 404, while the document they wanted sat one path over under a
// Kenwea-specific name. Being indexed and then failing the indexing request is a
// worse position than not being found.
func TestDiscoveryPathsCrawlersActuallyRequest(t *testing.T) {
	server := NewServer(StaticAuthenticator{})

	t.Run("the standard MCP discovery path serves the descriptor", func(t *testing.T) {
		for _, path := range []string{"/.well-known/mcp", "/mcp/v1/.well-known/mcp"} {
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s returned %d; crawlers ask for this path", path, rec.Code)
			}
			// Assert content rather than a 200, or an empty document would pass.
			for _, want := range []string{"com.kenwea.www/marketplace", "termsFingerprint", "streamable-http"} {
				if !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("%s is missing %q, so it is not the descriptor", path, want)
				}
			}
		}
	})

	t.Run("the Kenwea-specific path keeps working", func(t *testing.T) {
		// Anything already pointed at the old path must not break; the descriptor
		// itself cites it under verifyBeforeTrusting.
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, descriptorPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d", descriptorPath, rec.Code)
		}
	})

	t.Run("robots.txt is served and points at the descriptor", func(t *testing.T) {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("robots.txt returned %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "/.well-known/") {
			t.Fatalf("robots.txt does not allow the well-known space: %s", body)
		}
		if !strings.Contains(body, "Disallow: /mcp/v1") {
			t.Fatalf("robots.txt does not steer crawlers away from the JSON-RPC endpoint, which answers nothing to a GET: %s", body)
		}
	})

	// Deliberately still 404. These crawlers are asking "do you speak OAuth / A2A?"
	// and the answer is no; serving the documents would advertise a flow that cannot
	// be completed and a protocol that is not implemented, which is the exact defect
	// class this codebase spent the week removing.
	t.Run("protocols we do not speak stay absent rather than being faked", func(t *testing.T) {
		for _, path := range []string{
			"/.well-known/oauth-authorization-server",
			"/.well-known/oauth-protected-resource",
			"/.well-known/agent-card.json",
			"/agent.json",
		} {
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s returned %d; this server does not implement that protocol and must not claim to", path, rec.Code)
			}
		}
	})
}
