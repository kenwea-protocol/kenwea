package mcp

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

func TestRefusalTelemetryNamesTheReason(t *testing.T) {
	server := NewServer(StaticAuthenticator{})
	post := func(version, body, ua string) {
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", version)
		req.Header.Set("User-Agent", ua)
		server.ServeHTTP(httptest.NewRecorder(), req)
	}

	t.Run("an unsupported version is logged with its reason and the client", func(t *testing.T) {
		out := captureLog(t, func() { post("1999-01-01", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "probe/1") })
		if !strings.Contains(out, `mcp.refusal.telemetry status=400 reason="Unsupported protocol version" ua="probe/1"`) {
			t.Fatalf("expected a refusal line, got %q", out)
		}
	})

	t.Run("a header mismatch names which header", func(t *testing.T) {
		out := captureLog(t, func() {
			post(ProtocolStateless, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+statelessMeta+`}}`, "probe/2")
		})
		if !strings.Contains(out, `reason="Header mismatch: Mcp-Method header is required"`) {
			t.Fatalf("expected the header named, got %q", out)
		}
	})

	t.Run("a served request logs no refusal", func(t *testing.T) {
		out := captureLog(t, func() { post(ProtocolCurrent, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "probe/3") })
		if strings.Contains(out, "mcp.refusal.telemetry") {
			t.Fatalf("a 200 must not be logged as a refusal, got %q", out)
		}
	})

	t.Run("a crafted user agent cannot forge a second log line", func(t *testing.T) {
		out := captureLog(t, func() {
			post("1999-01-01", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "evil\nmcp.refusal.telemetry status=200")
		})
		if strings.Count(out, "\n") != 1 {
			t.Fatalf("expected exactly one log line, got %q", out)
		}
	})
}
