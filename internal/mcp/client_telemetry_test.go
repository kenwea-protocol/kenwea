package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// The permit half: the field we actually want must survive extraction intact.
// Everything else here is a refusal, and a sanitizer that returned "" for
// everything would pass all of them.
func TestInitializeClientInfoKeepsARealAnnouncement(t *testing.T) {
	info := initializeClientInfo(json.RawMessage(`{"protocolVersion":"2025-06-18","clientInfo":{"name":"@kenwea/mcp","version":"0.1.2"}}`))
	if info.Name != "@kenwea/mcp" || info.Version != "0.1.2" {
		t.Fatalf("a normal announcement must come through unchanged, got %+v", info)
	}
}

// A log a stranger can write is not evidence. A newline in the client name would
// let a caller forge a second telemetry line and invent traffic that never
// happened -- which is worse than having no telemetry, because it looks like data.
func TestInitializeClientInfoCannotForgeALogLine(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"clientInfo": map[string]any{
			"name":    "innocent\nmcp.client.telemetry name=\"very-popular-client\" version=\"9\"",
			"version": "1.0\r\n",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	info := initializeClientInfo(raw)
	if strings.ContainsAny(info.Name, "\n\r") || strings.ContainsAny(info.Version, "\n\r") {
		t.Fatalf("control characters must not survive: %q / %q", info.Name, info.Version)
	}
	if !strings.HasPrefix(info.Name, "innocent") {
		t.Errorf("the legitimate prefix should be kept rather than the whole value discarded, got %q", info.Name)
	}
}

// One connection must not be able to dominate the file the nightly snapshot has to
// read and merge.
func TestInitializeClientInfoCapsLength(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"clientInfo": map[string]any{"name": strings.Repeat("x", 500), "version": strings.Repeat("9", 500)},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	info := initializeClientInfo(raw)
	if len([]rune(info.Name)) > 64 || len([]rune(info.Version)) > 64 {
		t.Fatalf("values must be capped at 64 runes, got %d / %d", len([]rune(info.Name)), len([]rune(info.Version)))
	}
}

// Telemetry must never be able to break the thing it observes. `initialize` is the
// first call any client makes; a panic or an error here would make the server
// unusable for everyone, to collect a number nobody is waiting on.
func TestInitializeClientInfoToleratesGarbage(t *testing.T) {
	for _, params := range []string{
		``,
		`null`,
		`{}`,
		`{"clientInfo":null}`,
		`{"clientInfo":"a string, not an object"}`,
		`{"clientInfo":{"name":123}}`,
		`not json at all`,
		`{"clientInfo":{"name":"   "}}`,
	} {
		info := initializeClientInfo(json.RawMessage(params))
		if info.Name != "" {
			t.Errorf("params %q should yield no name, got %q", params, info.Name)
		}
	}
}
