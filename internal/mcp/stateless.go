package mcp

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// MCP revision 2026-07-28, served alongside the initialize-based revisions.
//
// The 2026-07-28 revision removed the initialize handshake and protocol sessions.
// Every request now carries its own protocol version and client capabilities in
// params._meta, the method (and, for tools/call, the tool name) is repeated in
// headers so a gateway can route without parsing the body, every result says
// resultType "complete", and the list results carry cache hints. The spec calls a
// server that answers both the old and the new shape "dual-era" and allows it on
// one endpoint: a request carrying modern _meta is served statelessly, and an
// initialize request selects the legacy semantics. That is what this file does.
// Nothing on the legacy path changes because of it.
//
// Source, read 2026-09-18: modelcontextprotocol.io/specification/2026-07-28,
// pages basic/index, basic/versioning, basic/transports/streamable-http,
// server/discover and server/utilities/caching.

// ProtocolStateless is the 2026-07-28 revision.
const ProtocolStateless = "2026-07-28"

// Per-request protocol fields, carried in params._meta.
const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// Error codes the 2026-07-28 revision defines. The spec reserves -32020 to -32099
// and forbids emitting any code in that range it does not define, so these three
// are the only ones this server uses there.
const (
	codeInvalidParams              = -32602
	codeHeaderMismatch             = -32020
	codeUnsupportedProtocolVersion = -32022
)

// The tool list is identical for every caller, because it does not depend on the
// credential presented, and it changes only when this server is redeployed. So it
// is public, and an hour is a conservative freshness window for a deploy cadence
// measured in days.
const (
	listCacheTTLMs   = 3600000
	listCacheScope   = "public"
	discoverCacheTTL = 3600000
)

// discoverInstructions is what a client that has never seen this server reads
// first. It says what the server is for and the order that works, and nothing it
// cannot back.
const discoverInstructions = "Kenwea exposes tools only. The first useful call is kenwea.sandbox.check, which notarizes what an artifact does and returns a verdict signed under a published Ed25519 key; it needs a key, which kenwea.onboarding.registerSelf issues in one keyless call with no human and no payment. tools/list and server/discover need no credential. Selling, buying and bidding require an agent claimed by a human operator."

func serverInfo() map[string]string {
	return map[string]string{"name": "kenwea-public-mcp", "version": "1.0.0"}
}

// requestMeta is what a request's params._meta says about the caller.
type requestMeta struct {
	protocolVersion string
	hasVersion      bool
	hasCapabilities bool
	client          clientInfo
}

func readRequestMeta(params json.RawMessage) requestMeta {
	var out requestMeta
	if len(params) == 0 || firstJSONToken(params) != '{' {
		return out
	}
	var body struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &body); err != nil || body.Meta == nil {
		return out
	}
	if raw, ok := body.Meta[metaProtocolVersion]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			out.protocolVersion = v
			out.hasVersion = true
		}
	}
	if raw, ok := body.Meta[metaClientCapabilities]; ok && firstJSONToken(raw) == '{' {
		out.hasCapabilities = true
	}
	if raw, ok := body.Meta[metaClientInfo]; ok {
		var info struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &info) == nil {
			out.client = clientInfo{Name: sanitizeTelemetryValue(info.Name), Version: sanitizeTelemetryValue(info.Version)}
		}
	}
	return out
}

// isStatelessRequest decides the era of one request, and the header decides it.
//
// initialize always selects the legacy semantics, whatever header came with it,
// because that is what the spec says it means. Otherwise a request is modern when
// its header names 2026-07-28, or when it has no header at all but carries a
// protocol version in _meta, which is a modern request that forgot the header and
// is refused as one.
//
// A request whose header names an older revision is legacy even if it carries
// modern _meta. That is deliberate. Our own published stdio bridge, @kenwea/mcp
// 0.2.5, stamps its configured legacy version on every request it forwards, so a
// modern client speaking through it arrives with an old header and new _meta.
// Before this revision existed on the server that request was served, because
// _meta was ignored. Refusing it now as a header mismatch would have broken a
// working client to enforce a rule meant for requests that claim to be modern.
func isStatelessRequest(r *http.Request, req rpcRequest, meta requestMeta) bool {
	if req.Method == "initialize" {
		return false
	}
	header := r.Header.Get("MCP-Protocol-Version")
	return header == ProtocolStateless || (header == "" && meta.hasVersion)
}

// serveStateless answers one request under the 2026-07-28 rules.
func (s *Server) serveStateless(w http.ResponseWriter, r *http.Request, req rpcRequest, meta requestMeta) {
	// A notification gets 202 and no body. The revision defines no header
	// requirements for notification POSTs, so none are checked.
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// The per-request fields come first: without them there is nothing to
	// compare the headers against.
	if !meta.hasVersion || !meta.hasCapabilities {
		var missing []string
		if !meta.hasVersion {
			missing = append(missing, metaProtocolVersion)
		}
		if !meta.hasCapabilities {
			missing = append(missing, metaClientCapabilities)
		}
		writeProtocolError(w, http.StatusBadRequest, req.ID, codeInvalidParams, "Invalid params", map[string]any{
			"detail":  "a request without initialize must carry its protocol version and client capabilities in params._meta",
			"missing": missing,
		})
		return
	}
	if meta.protocolVersion != ProtocolStateless {
		writeUnsupportedVersion(w, req.ID, meta.protocolVersion)
		return
	}

	// Headers must repeat the body. A gateway routes on them, so a request whose
	// headers and body disagree would be routed as one thing and executed as
	// another.
	if detail := headerMismatch(r, req, meta); detail != "" {
		writeProtocolError(w, http.StatusBadRequest, req.ID, codeHeaderMismatch, "Header mismatch", map[string]string{"detail": detail})
		return
	}

	// Sessions do not exist in this revision. A stale session id from an older
	// client is ignored rather than honoured, so it can neither authenticate this
	// request nor be refused as invalid, and none is minted in reply.
	r.Header.Del("Mcp-Session-Id")

	switch req.Method {
	case "server/discover":
		logStatelessClient(meta)
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: completeResult(map[string]any{
			"supportedVersions": SupportedProtocolVersionList(),
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      discoverInstructions,
			"ttlMs":             discoverCacheTTL,
			"cacheScope":        listCacheScope,
		})})
		return
	case "tools/list":
		logStatelessClient(meta)
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: completeResult(map[string]any{
			"tools":      mcpToolDescriptors(),
			"ttlMs":      listCacheTTLMs,
			"cacheScope": listCacheScope,
		})})
		return
	case "tools/call":
		s.serveTool(w, r, req, true)
		return
	}
	if allowedTool(req.Method) {
		// A direct kenwea.* JSON-RPC call, the convenience form this server has
		// always accepted. The revision's _meta is protocol plumbing, not an
		// argument, so it is not forwarded to the platform.
		req.Params = withoutMeta(req.Params)
		s.serveTool(w, r, req, true)
		return
	}
	// Everything else, including ping, logging/setLevel and the resources and
	// prompts methods, is not a method this server has under this revision.
	writeStatelessMethodNotFound(w, req.ID, "this server implements the tools capability only; see server/discover")
}

// headerMismatch returns why the routing headers disagree with the body, or "".
func headerMismatch(r *http.Request, req rpcRequest, meta requestMeta) string {
	version := r.Header.Get("MCP-Protocol-Version")
	if version == "" {
		return "MCP-Protocol-Version header is required"
	}
	if version != meta.protocolVersion {
		return "MCP-Protocol-Version header does not match params._meta"
	}
	method, ok := decodeHeaderValue(r.Header.Get("Mcp-Method"))
	if !ok || method == "" {
		return "Mcp-Method header is required"
	}
	if method != req.Method {
		return "Mcp-Method header does not match the request method"
	}
	switch req.Method {
	case "tools/call", "prompts/get", "resources/read":
		var target struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		}
		_ = json.Unmarshal(req.Params, &target)
		want := target.Name
		if req.Method == "resources/read" {
			want = target.URI
		}
		name, ok := decodeHeaderValue(r.Header.Get("Mcp-Name"))
		if !ok || name == "" {
			return "Mcp-Name header is required for " + req.Method
		}
		if name != want {
			return "Mcp-Name header does not match the request"
		}
	}
	return ""
}

// decodeHeaderValue undoes the encoding the revision specifies for values that
// are not header-safe: =?base64?<value>?=. A value that is not in that form is
// taken as it is.
func decodeHeaderValue(v string) (string, bool) {
	const prefix, suffix = "=?base64?", "?="
	if strings.HasPrefix(v, prefix) && strings.HasSuffix(v, suffix) && len(v) >= len(prefix)+len(suffix) {
		decoded, err := base64.StdEncoding.DecodeString(v[len(prefix) : len(v)-len(suffix)])
		if err != nil {
			return "", false
		}
		return string(decoded), true
	}
	return v, true
}

// completeResult marks a result as finished and names the server on it. The
// revision requires resultType on every result and asks for serverInfo in each
// result's _meta. The input is copied, never modified, because a direct call's
// result is the platform's own map.
func completeResult(result any) map[string]any {
	out := map[string]any{}
	if m, ok := result.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	out["resultType"] = "complete"
	meta := map[string]any{}
	if existing, ok := out["_meta"].(map[string]any); ok {
		for k, v := range existing {
			meta[k] = v
		}
	}
	meta[metaServerInfo] = serverInfo()
	out["_meta"] = meta
	return out
}

// withoutMeta drops params._meta from a JSON object and returns everything else
// unchanged. Anything that is not an object is returned as it is.
func withoutMeta(params json.RawMessage) json.RawMessage {
	if len(params) == 0 || firstJSONToken(params) != '{' {
		return params
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil {
		return params
	}
	if _, ok := fields["_meta"]; !ok {
		return params
	}
	delete(fields, "_meta")
	out, err := json.Marshal(fields)
	if err != nil {
		return params
	}
	return out
}

// writeUnsupportedVersion is the answer to a protocol version this server does not
// speak, in either era. It uses the shape the 2026-07-28 revision defines, because
// a client that speaks both eras reads a 400 body to decide whether it reached a
// modern server, and an unrecognised body sends it back to initialize. The reason
// string is kept so the legacy wording still appears.
func writeUnsupportedVersion(w http.ResponseWriter, id any, requested string) {
	writeProtocolError(w, http.StatusBadRequest, id, codeUnsupportedProtocolVersion, "Unsupported protocol version", map[string]any{
		"supported": SupportedProtocolVersionList(),
		"requested": requested,
		"reason":    "unsupported_protocol_version",
		"detail":    "unsupported MCP protocol version; this server speaks " + strings.Join(SupportedProtocolVersionList(), ", "),
	})
}

func writeProtocolError(w http.ResponseWriter, status int, id any, code int, message string, data any) {
	writeJSON(w, status, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcErrorObj{Code: code, Message: message, Data: data},
	})
}

// writeStatelessMethodNotFound differs from the legacy answer only in the HTTP
// status: the 2026-07-28 transport says an unknown method is 404, where the legacy
// path deliberately answers 200 so older clients do not read it as a broken
// transport.
func writeStatelessMethodNotFound(w http.ResponseWriter, id any, detail string) {
	writeJSON(w, http.StatusNotFound, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcErrorObj{
			Code:    -32601,
			Message: "Method not found",
			Data:    map[string]string{"detail": detail},
		},
	})
}

// logStatelessClient keeps the client census working after initialize is gone.
// Under the legacy era a client announced itself once, on initialize; under this
// one it may announce itself on every request, so the census is taken on the two
// requests a new client makes first.
func logStatelessClient(meta requestMeta) {
	name := meta.client.Name
	if name == "" {
		name = "(unannounced)"
	}
	log.Printf("mcp.client.telemetry name=%q version=%q era=%q", name, meta.client.Version, ProtocolStateless)
}
