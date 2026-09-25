package mcp

import (
	"encoding/json"
	"strings"
)

// Who is actually connecting.
//
// Every MCP client announces itself in `initialize` -- `clientInfo.name` and
// `.version` are part of the base protocol and arrive on the very first call. We
// discarded that field for the entire life of this server, and it is the reason a
// question we badly needed answered could not be:
//
// Measured 2026-08-06: 73 distinct clients using a `node` user agent had connected
// to mcp.kenwea.com, ~130 requests a day, and not one had ever called a tool. Two
// explanations fit that evidence exactly and imply opposite work. Either people ran
// our npm bridge, looked at the tool list and found nothing worth calling -- which
// is a product problem -- or every one of them was a package scanner, which is a
// distribution problem. The user agent cannot tell them apart, because our own
// bridge is a node process too. `clientInfo.name` can, and we were throwing it away.
//
// WHAT THIS DELIBERATELY DOES NOT RECORD. Not the IP, not the actor, not the
// params, and nothing that joins this to a specific caller. `clientInfo.name` is the
// name of a piece of SOFTWARE ("claude-ai", "mcp-inspector", "@kenwea/mcp"), which
// is the same class of fact as a user agent -- not an identity. That distinction is
// what keeps this consistent with the promise made in the tourist tier: browsing is
// counted in aggregate and never attributed to who did it.
//
// The honest caveat, stated because it is a real one: a caller free-typing a unique
// name ("acme-internal-agent-v3") identifies itself by doing so. We cannot prevent
// that -- it is the client's own choice of what to announce -- and we do not make it
// worse by joining it to anything else.
//
// DURABILITY. This goes to stdout, which a redeploy erases. That is not an oversight
// left for later: this server has no database and no volume by design, and adding
// either for telemetry would be a real architectural change bought with a
// second-order need. The nightly host snapshot (scripts/mcp-traffic-snapshot.sh)
// harvests these lines and merges them into a cumulative file instead, so the loss
// window is bounded by "whatever arrived since the last snapshot" rather than "all
// of it".

// clientInfo is the sanitized announcement a client makes in `initialize`.
type clientInfo struct {
	Name    string
	Version string
}

// initializeClientInfo extracts the client's self-announcement. Like
// publishSourceFramework, it never rejects the call and never lets a crafted value
// corrupt a log line: telemetry must not be able to break the thing it observes.
func initializeClientInfo(params json.RawMessage) clientInfo {
	if len(params) == 0 {
		return clientInfo{}
	}
	var body struct {
		ClientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return clientInfo{}
	}
	return clientInfo{
		Name:    sanitizeTelemetryValue(body.ClientInfo.Name),
		Version: sanitizeTelemetryValue(body.ClientInfo.Version),
	}
}

// sanitizeTelemetryValue strips control characters -- a newline here would let a
// caller forge a second log line, and a log a stranger can write is not evidence --
// and caps the length so one connection cannot dominate a file the snapshot has to
// read.
func sanitizeTelemetryValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if runes := []rune(value); len(runes) > 64 {
		value = string(runes[:64])
	}
	return value
}
