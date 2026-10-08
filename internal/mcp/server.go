package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/idempotency"
	idempotencytest "github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/idempotency/teststore"
	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/session"
	sessiontest "github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/session/teststore"
)

const (
	ProtocolCurrent = "2025-11-25"
	ProtocolCompat  = "2025-03-26"
	// A published revision between the two above. It was missing from the accepted
	// set, so clients speaking it -- a large share of them -- were refused with
	// "unsupported" for a version this server can in fact serve.
	ProtocolIntermediate = "2025-06-18"
)

// The set the request handler checks, rather than a chain of != comparisons. A
// comparison chain is where the 2026-07-30 defect lived: adding a revision meant
// editing a boolean expression, and nobody did, so the newest accepted version was
// the one someone happened to hardcode in two places.
var supportedProtocolVersions = map[string]bool{
	ProtocolStateless:    true,
	ProtocolCurrent:      true,
	ProtocolIntermediate: true,
	ProtocolCompat:       true,
}

// SupportedProtocolVersionList returns the accepted revisions, newest first. It is
// the single source for the error message, the capability descriptor's
// transport.protocolVersions, server/discover's supportedVersions, and the tests --
// places that previously carried their own copies and could disagree without
// anything failing.
//
// ProtocolCurrent stays the newest revision that uses initialize; the 2026-07-28
// revision is ProtocolStateless and is served by stateless.go.
func SupportedProtocolVersionList() []string {
	return []string{ProtocolStateless, ProtocolCurrent, ProtocolIntermediate, ProtocolCompat}
}

// legacyVersionFor answers initialize the way the initialize-based revisions
// require: with the version the client asked for when this server speaks it, and
// otherwise with the newest one it does speak. Until 2026-09-18 it always answered
// 2025-11-25, so a client that asked for 2025-06-18 was told a version it had not
// offered.
func legacyVersionFor(params json.RawMessage) string {
	var body struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &body) == nil {
		switch body.ProtocolVersion {
		case ProtocolCurrent, ProtocolIntermediate, ProtocolCompat:
			return body.ProtocolVersion
		}
	}
	return ProtocolCurrent
}

type Actor struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	OperatorID string `json:"operatorId,omitempty"`
	AgentID    string `json:"agentId,omitempty"`
}

type AuthResult struct {
	Actor   Actor
	Policy  AgentPolicy
	Revoked bool
}

type Authenticator interface {
	Authenticate(*http.Request) (AuthResult, error)
}

type ToolForwarder interface {
	ForwardTool(*http.Request, string, json.RawMessage) (map[string]any, error)
}

// PlatformError carries the real status and coded reason the Platform API
// returned for a forwarded tool call. The adapter uses it to relay a client
// error (4xx) back to the agent with the platform's own code, instead of
// masking every non-2xx as a generic gateway outage — which would tell a
// well-behaved agent to back off and retry a request it should have corrected.
type PlatformError struct {
	StatusCode int
	Code       string
	Detail     string
}

func (e *PlatformError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return e.Code
}

type StaticAuthenticator struct {
	Actor   Actor
	Policy  AgentPolicy
	Revoked bool
	Err     error
}

func (a StaticAuthenticator) Authenticate(*http.Request) (AuthResult, error) {
	if a.Err != nil {
		return AuthResult{}, a.Err
	}
	return AuthResult{Actor: a.Actor, Policy: a.Policy, Revoked: a.Revoked}, nil
}

type Server struct {
	auth        Authenticator
	sessions    session.Store
	idempotency idempotency.Store
	forwarder   ToolForwarder
}

func NewServer(auth Authenticator) *Server {
	return NewServerWithStores(auth, sessiontest.New(), idempotencytest.New())
}

func NewServerWithStores(auth Authenticator, sessions session.Store, idem idempotency.Store) *Server {
	return &Server{auth: auth, sessions: sessions, idempotency: idem}
}

func NewServerWithRuntime(auth Authenticator, sessions session.Store, idem idempotency.Store, forwarder ToolForwarder) *Server {
	return &Server{auth: auth, sessions: sessions, idempotency: idem, forwarder: forwarder}
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	// The notary server has its own handler and its own two tools; nothing on the
	// marketplace path below is reachable through it. See notary.go.
	if r.URL.Path == notaryPath || r.URL.Path == notaryPath+"/health" {
		s.serveNotary(rw, r)
		return
	}
	var w http.ResponseWriter = rw
	if r.URL.Path == "/mcp/v1" {
		w = &refusalRecorder{ResponseWriter: rw, ua: sanitizeTelemetryValue(r.UserAgent())}
	}
	if s.handleUtilityRoute(w, r) {
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		writeHTTPError(w, http.StatusMethodNotAllowed, nil, "method_not_allowed", "unsupported HTTP method")
		return
	}
	if !allowedOrigin(r.Header.Get("Origin")) {
		writeHTTPError(w, http.StatusForbidden, nil, "origin_rejected", "origin is not allowed for public MCP")
		return
	}
	// The 2026-07-28 revision has no sessions to delete and no GET stream to open;
	// its transport is a single POST endpoint. A client that names that revision on
	// GET or DELETE is told so, while the older revisions keep both.
	if r.Method != http.MethodPost && r.Header.Get("MCP-Protocol-Version") == ProtocolStateless {
		w.Header().Set("Allow", http.MethodPost)
		writeHTTPError(w, http.StatusMethodNotAllowed, nil, "method_not_allowed", "revision "+ProtocolStateless+" is stateless: POST only, with no sessions and no GET stream")
		return
	}
	if r.Method == http.MethodDelete {
		sessionID := r.Header.Get("Mcp-Session-Id")
		if sessionID == "" {
			writeHTTPError(w, http.StatusUnauthorized, nil, "invalid_session", "missing MCP session id")
			return
		}
		if err := s.sessions.Delete(sessionID); err != nil {
			writeHTTPError(w, http.StatusInternalServerError, nil, "session_delete_failed", "failed to delete MCP session")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "session_terminated"})
		return
	}
	if r.Method == http.MethodGet {
		if sessionID := r.Header.Get("Mcp-Session-Id"); sessionID != "" {
			if _, ok := s.sessions.Get(sessionID); !ok {
				writeHTTPError(w, http.StatusUnauthorized, nil, "invalid_session", "MCP session is invalid")
				return
			}
		}
		streamReady(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	// Measured 2026-07-30 from mcp.kenwea.com's access log: 54 of 104 requests to
	// this endpoint were rejected 400, against 29 that succeeded. Both causes were
	// here.
	//
	// An absent header was refused. It must not be: a client's first request is
	// `initialize`, at which point it has not negotiated a version yet, so the
	// header is legitimately missing. The spec's instruction is to assume the
	// backwards-compatible revision rather than refuse. Refusing meant every
	// first contact from a spec-following client failed -- and the crawlers that
	// index MCP servers are exactly first contacts.
	//
	// And the accepted set was two exact strings, which excluded 2025-06-18 -- a
	// real published revision that many clients send. So a correct client speaking
	// a version we could have served was told we could not speak it.
	//
	// This was invisible for the same reason as the rest of this week's findings:
	// our own bridge hardcodes the same two strings, so every test we ran sent a
	// version we accepted. The check could only fail for someone we had not written.
	//
	// The check itself now runs after the body is parsed, below: since 2026-07-28 a
	// request can carry its version in params._meta, so the era of a request is not
	// known until the body is read.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, nil, "invalid_json", "could not read request body")
		return
	}
	// A JSON array is a batched JSON-RPC request. This server takes one request per
	// POST, but calling a well-formed array "invalid JSON" is simply untrue, and a
	// caller told its JSON is broken checks its serialiser rather than its framing.
	// Measured 2026-08-06 by sending one: we answered 400 invalid_json.
	if firstJSONToken(body) == '[' {
		writeRPCError(w, http.StatusOK, nil, "batch_not_supported",
			"this server takes one JSON-RPC request per POST; send the requests separately")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, nil, "invalid_json", "invalid JSON-RPC request")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		// Arrived intact, is not a valid JSON-RPC request. By the same rule as the
		// unknown-method branch below, that is the envelope's business and not the
		// transport's: -32600 over 200. Leaving this one on 400 while fixing the
		// others would have been the instances-not-the-shape mistake a third time,
		// in the commit complaining about it happening twice.
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &rpcErrorObj{
				Code:    -32600,
				Message: "Invalid Request",
				Data:    map[string]string{"detail": "a request must carry jsonrpc:\"2.0\" and a method"},
			},
		})
		return
	}
	meta := readRequestMeta(req.Params)
	if isStatelessRequest(r, req, meta) {
		s.serveStateless(w, r, req, meta)
		return
	}
	version := r.Header.Get("MCP-Protocol-Version")
	if version == "" {
		version = ProtocolCompat
	}
	if !supportedProtocolVersions[version] {
		writeUnsupportedVersion(w, req.ID, version)
		return
	}
	if s.handleMCPProtocolMethod(w, r, req) {
		return
	}
	s.serveTool(w, r, req, false)
}

// serveTool runs one tool request through the gates and the forwarder. Both eras
// share it, so a gate cannot exist on one path and be missing on the other. The
// stateless era differs in three places only: an unknown direct method is a 404,
// no session is read or minted, and the result is marked complete.
func (s *Server) serveTool(w http.ResponseWriter, r *http.Request, req rpcRequest, stateless bool) {
	wrapToolResult := false
	if req.Method == "tools/call" {
		method, params, err := decodeMCPToolCall(req.Params)
		if err != nil {
			writeRPCError(w, http.StatusBadRequest, req.ID, "invalid_tool_call", err.Error())
			return
		}
		req.Method = method
		req.Params = params
		wrapToolResult = true
	}
	// Resolve an older name to the tool it stands for before any gate reads it,
	// so every gate below sees exactly one name per tool.
	req.Method = canonicalTool(req.Method)
	if !allowedTool(req.Method) {
		// A well-formed request for something this server does not have is answered
		// over a SUCCESSFUL transport, with the JSON-RPC word for the situation.
		//
		// This generalises the fix made on 2026-07-31, which enumerated the seven
		// standard methods that had shown up in the access log and left everything
		// else on the old path. Measured 2026-08-06: probes still get HTTP 400 for
		// any method not on that list -- `io.verifymcp`, `MCPScoringEngine` and
		// `mcpgrade-probe` send unrecognised methods deliberately, precisely to see
		// whether a server answers the protocol or breaks the transport, and we were
		// failing that check ~40 times a day while telling them the method was
		// "outside active public MCP scope", which reads as a policy refusal rather
		// than an absence.
		//
		// Fixing the instances instead of the shape is the defect this codebase kept
		// finding all week; this is the same mistake, made in the fix for it.
		if wrapToolResult {
			writeRPCMethodNotFound(w, req.ID, "no tool by that name; call tools/list for the tools this server exposes")
			return
		}
		if stateless {
			writeStatelessMethodNotFound(w, req.ID, "this server implements the tools capability only; see server/discover")
			return
		}
		writeRPCMethodNotFound(w, req.ID, "this server implements the tools capability only; see the capabilities block returned by initialize")
		return
	}
	if requiresFreshAuthorization(r, req.Method) {
		writeRPCError(w, http.StatusUnauthorized, req.ID, "authorization_required", "mutating MCP tools require fresh Authorization so revoked keys cannot continue through a cached session")
		return
	}
	if req.Method == "kenwea.onboarding.registerSelf" {
		result, err := s.resultForRequest(r, req, Actor{})
		if err != nil {
			writeForwardError(w, req.ID, err)
			return
		}
		// Counted here, at the anonymous entry point, so the funnel's first step
		// (registrations) can be compared against its second (tourist tool calls).
		toolUsage.recordRegistration()
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: eraResult(responseResult(result, wrapToolResult), stateless)})
		return
	}
	auth, err := s.authenticate(w, r, req.ID, !stateless)
	if err != nil {
		return
	}
	if auth.Revoked {
		writeHTTPError(w, http.StatusUnauthorized, req.ID, "revoked_key", "agent key has been revoked")
		return
	}
	if lowPriorityTool(req.Method) && r.Header.Get("X-Kenwea-Backpressure-Level") == "critical" {
		writeRPCError(w, http.StatusServiceUnavailable, req.ID, "backpressure_shed", "low-priority intelligence traffic was shed before critical paths")
		return
	}
	if err := rejectActorSpoof(auth.Actor, req.Params); err != nil {
		writeRPCError(w, http.StatusForbidden, req.ID, "actor_confusion_rejected", err.Error())
		return
	}
	if err := rejectUnboundMutatingAgent(auth.Actor, req.Method); err != nil {
		writeRPCError(w, http.StatusForbidden, req.ID, "operator_required", err.Error())
		return
	}
	if requiresIdempotency(req.Method) && idempotencyKey(r, req.Params) == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, "idempotency_required", "financial or destructive MCP tool requires Idempotency-Key")
		return
	}
	if err := validateToolParams(req.Method, req.Params); err != nil {
		writeRPCError(w, http.StatusBadRequest, req.ID, "validation_failed", err.Error())
		return
	}
	if err := enforceOperatorPolicy(auth.Policy, req.Method, req.Params, auth.Actor.Type == "agent" && auth.Actor.OperatorID == ""); err != nil {
		writeRPCError(w, http.StatusForbidden, req.ID, policyCode(err), err.Error())
		return
	}
	if req.Method == "kenwea.marketplace.publish" {
		if sf := publishSourceFramework(req.Params); sf != "" {
			// Structured telemetry: which agent framework a seller published from.
			// Observable in `docker logs` today; durable aggregation is a Platform
			// API follow-up. Never authorization-bearing.
			log.Printf("mcp.publish.telemetry source_framework=%q actor=%s", sf, auth.Actor.ID)
		}
	}
	// Aggregate usage only: tool name and caller tier, never the actor id and
	// never params. Placed after all the gates so it records real served calls,
	// not rejected attempts.
	//
	// The actor id was here for one day and was removed deliberately. It made a
	// specific agent's browsing traceable, which is instrumenting the VISITOR
	// rather than the artifact -- and an unwatched tourist is part of what makes
	// the no-commitment path trustworthy in the first place. Publishing a keyless
	// "come look around" surface while quietly recording who looked at what is
	// exactly the claim/behaviour gap this codebase refuses elsewhere. The
	// funnel question ("does the tourist tier get used, and for what?") is fully
	// answered by these aggregates; "which agent browsed?" is curiosity, and it
	// costs more trust than it buys. The guarantee is now published in the
	// capability descriptor's neverDoes list, so it is a commitment, not a habit.
	tier := actorTier(auth.Actor)
	toolUsage.record(req.Method, tier)
	log.Printf("mcp.tool.telemetry tool=%q tier=%s", req.Method, tier)
	result, err := s.resultForRequest(r, req, auth.Actor)
	if err != nil {
		writeForwardError(w, req.ID, err)
		return
	}
	if key := idempotencyKey(r, req.Params); key != "" {
		resolved, _, conflict, err := s.idempotency.Resolve(auth.Actor.ID+":"+key, requestHash(req.Params), result)
		if err != nil {
			writeRPCError(w, http.StatusServiceUnavailable, req.ID, "idempotency_unavailable", "idempotency store unavailable")
			return
		}
		if conflict {
			writeRPCError(w, http.StatusConflict, req.ID, "idempotency_conflict", "idempotency key was reused with a different request hash")
			return
		}
		result, _ = resolved.(map[string]any)
	}
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  eraResult(responseResult(result, wrapToolResult), stateless),
	})
}

// eraResult leaves a legacy result exactly as it was and marks a stateless one
// complete.
func eraResult(result any, stateless bool) any {
	if !stateless {
		return result
	}
	return completeResult(result)
}

func (s *Server) handleMCPProtocolMethod(w http.ResponseWriter, r *http.Request, req rpcRequest) bool {
	switch req.Method {
	case "initialize":
		// Logged before the response is written, so a client that announces itself
		// and then disconnects is still counted. See client_telemetry.go for what
		// this deliberately does not record.
		if info := initializeClientInfo(req.Params); info.Name != "" {
			log.Printf("mcp.client.telemetry name=%q version=%q", info.Name, info.Version)
		} else {
			// A client that announces nothing is itself the finding: `clientInfo` is
			// required by the protocol, so an empty one means a hand-rolled prober
			// rather than a real MCP client library.
			log.Printf("mcp.client.telemetry name=%q version=%q", "(unannounced)", "")
		}
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": legacyVersionFor(req.Params),
				"capabilities": map[string]any{
					"tools": map[string]any{"listChanged": false},
				},
				"serverInfo": serverInfo(),
			},
		})
		return true
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return true
	case "tools/list":
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{"tools": mcpToolDescriptors()},
		})
		return true
	// A base-protocol liveness check, not a capability. Answering it costs nothing
	// and refusing it is actively harmful: a client that pings and gets an error
	// has to decide whether the server is broken, and some drop the connection.
	case "ping":
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
		return true

	// Standard methods this server does not implement.
	//
	// Measured on production 2026-07-31: 14 of the day's remaining 400s were these,
	// and they are what a conforming client asks for immediately after initialize.
	// The old answer was HTTP 400 with a Kenwea-specific `method_not_allowed`, which
	// tells the client the TRANSPORT failed -- so a well-behaved caller doing the
	// standard thing concluded the server was broken rather than that the capability
	// was absent.
	//
	// The fix is deliberately not "return an empty list". An empty `resources` array
	// says the capability exists and happens to hold nothing; that is not true here,
	// and this codebase does not make claims it cannot back. JSON-RPC already has
	// the exact word for the true situation -- -32601, method not found -- carried
	// over a successful HTTP transaction, which is the shape every client already
	// knows how to read. The capabilities block in `initialize` announces `tools`
	// and nothing else, so this answer agrees with what we advertised.
	case "resources/list", "resources/templates/list", "resources/read",
		"prompts/list", "prompts/get", "completion/complete", "logging/setLevel":
		writeRPCMethodNotFound(w, req.ID,
			"this server implements the tools capability only; see the capabilities block returned by initialize")
		return true

	case "tools/call":
		return false
	default:
		return false
	}
}

func decodeMCPToolCall(params json.RawMessage) (string, json.RawMessage, error) {
	var body struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) == 0 || string(params) == "null" {
		return "", nil, errors.New("tools/call requires name and arguments")
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return "", nil, errors.New("invalid tools/call params")
	}
	if strings.TrimSpace(body.Name) == "" {
		return "", nil, errors.New("tools/call requires tool name")
	}
	if len(body.Arguments) == 0 {
		body.Arguments = json.RawMessage(`{}`)
	}
	return body.Name, body.Arguments, nil
}

func responseResult(result map[string]any, wrapToolResult bool) any {
	if !wrapToolResult {
		return result
	}
	payload, _ := json.Marshal(result)
	return map[string]any{
		"content": []map[string]string{
			{"type": "text", "text": string(payload)},
		},
		"structuredContent": result,
	}
}

func mcpToolDescriptors() []map[string]any {
	names := make([]string, 0, len(allowedTools))
	// allowedTools holds listed names only; unlistedAliases still answer but
	// are not offered to a caller choosing what to call.
	for name := range allowedTools {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]map[string]any, 0, len(names))
	for _, name := range names {
		descriptor := map[string]any{
			"name":        name,
			"description": toolDescription(name),
			"inputSchema": toolInputSchema(name),
			"annotations": toolAnnotations(name),
		}
		if title := toolTitle(name); title != "" {
			descriptor["title"] = title
		}
		// Omitted rather than guessed where the shape is unverified: declaring an
		// outputSchema obliges us to return structuredContent that conforms, and a
		// wrong promise breaks the callers careful enough to check.
		if out := toolOutputSchema(name); out != nil {
			descriptor["outputSchema"] = out
		}
		tools = append(tools, descriptor)
	}
	return tools
}

// toolDescription is the one sentence an agent reads before deciding whether to call
// a tool. Until 2026-07-30 twenty of the twenty-nine tools fell through to the default
// "Kenwea public MCP tool.", which names the vendor and says nothing about the tool --
// so choosing between kenwea.marketplace.install and kenwea.marketplace.purchase meant
// calling one and reading the error. Each description below says what the tool does and,
// where it matters, what it costs or what it requires that the schema cannot express.
func toolDescription(name string) string {
	switch name {

	case "kenwea.onboarding.registerSelf":
		return "Self-register as a new agent with no credential and no human. Send agentName (not name, which is ignored); declaredModel is optional and shown as your own claim, never verified. Returns a one-time API key and a pairing PIN. Use this first if you have no Kenwea key. The key can browse the whole market and run kenwea.sandbox.check immediately; selling, buying and bidding wait until a human operator claims you with the PIN. Operators creating an agent for themselves use kenwea.onboarding.startOperatorAgent instead."
	case "kenwea.onboarding.startOperatorAgent":
		return "For operators only: create a new agent under the calling operator and issue its first API key. agentName is the new agent's display name and keyLabel names the key (default Initial). Requires an operator session or an operator-bound agent key; the new agent belongs to that operator from the start, so it needs no pairing PIN. Reuse the same idempotencyKey to retry without creating a second agent. An agent registering itself uses kenwea.onboarding.registerSelf instead."

	case "kenwea.agent.getIdentity":
		return "Read who you are on Kenwea. Takes no arguments and returns your actor: its type, its id, its agentId and, once a human operator has claimed you, its operatorId. Call it first when a write is refused with operator_required: no operatorId means you must be claimed (kenwea.onboarding.registerSelf gave you the pairing PIN for that); an operatorId means your operator has not granted that permission. Read-only and free. (kenwea.agent.identity, kenwea.auth.identify and kenwea.auth.profile are older names for this tool and still answer.)"
	case "kenwea.agent.sendHeartbeat":
		return "Record that this agent is alive. Takes no arguments, returns status accepted, and only updates your last-seen time, which your operator sees. It moves no money and changes nothing else, so repeating it is harmless; call it on a schedule while you run. It checks nothing: for platform load use kenwea.scale.getStatus, for a job you started use kenwea.jobs.getStatus."

	case "kenwea.marketplace.search":
		return "Find products listed on the Kenwea marketplace. q matches title, category and summary; category is an exact match; minPriceCents and maxPriceCents bound the price in cents (0 means no bound); sort picks the order and defaults to best-selling. Page with limit (1 to 100, default 50) and offset. Returns products plus topSoldProducts and topRequestedCategories. For items similar to one product use kenwea.recommendations.listRelatedProducts; for what buyers want but cannot find use kenwea.analytics.getForecast. Readable by unclaimed agents."
	case "kenwea.marketplace.preview":
		return "Run the seller's demo of one listed product before buying it, in a sandbox with no network, no capabilities and a read-only filesystem. productId is a product id from kenwea.marketplace.search. Asynchronous: it returns a jobId, and the demo's output arrives through kenwea.jobs.getStatus. A product whose seller supplied no demo fails with no_preview_demo; one with no live, sandbox-approved version fails with product_not_previewable. Free, creates no purchase, requires an operator-claimed agent. To check a file or package that is not a Kenwea listing, use kenwea.sandbox.check instead."
	case "kenwea.marketplace.publish":
		return "List a product for sale. Asynchronous: returns a jobId; poll kenwea.jobs.getStatus, whose result then names the productId, productVersionId and listingStatus. Publishing a title you already sell adds a new version to that product, so version must not repeat (a repeated version fails the job). category must be one of the enum values, every image needs url and altText, and sellerAgreementAccepted must be true, or the call fails before any job starts. priceCents must be 0 or your operator's fixed price unless dynamic pricing is delegated (pricing_policy_denied otherwise). The optional preview object becomes the demo buyers can run. The artifact is sandbox-checked before it can go live; an unclaimed agent's listing stays a draft no buyer can see."
	case "kenwea.marketplace.purchase":
		return "Buy a specific product version. THIS SPENDS MONEY from the agent wallet: the wallet must cover the price (insufficient_wallet_balance otherwise; check it with kenwea.wallet.getBalance), and the price counts against the operator's daily budget (budget_exceeded when it would go over). productVersionId is a product VERSION id, not a product id; find it with kenwea.marketplace.search or kenwea.marketplace.preview. A version that has not passed the sandbox is refused with sandbox_not_approved. A price of 1000 USDT or more waits for operator approval and moves no money until then. Returns a LicenseID; to put the product to use, call kenwea.marketplace.install with it."
	case "kenwea.marketplace.install":
		return "Install a product this agent has bought. licenseId is the LicenseID kenwea.marketplace.purchase returned; runtime is optional, and if the product's manifest requires a different runtime the call fails with compatibility_failed (runtime_mismatch) and installs nothing. Fails with license_required when the license is not active or not yours. Spends nothing and returns an InstallationID. Repeating the call with the same idempotencyKey returns the same installation; a new key records another one."

	case "kenwea.wallet.getBalance":
		return "Read this agent's spendable balance: balanceCents in USDT cents, computed from the ledger. terms states the rules: spend only, no withdrawal in this version, no expiry. Takes no arguments and is read-only. For the individual credits and debits use kenwea.wallet.listTransactions."
	case "kenwea.wallet.listTransactions":
		return "List this agent's 50 most recent wallet ledger entries, newest first: every credit and debit behind the balance, with its type and amount in cents. Takes no arguments and has no paging. For the current total use kenwea.wallet.getBalance; for which products were bought or passed over and why, use kenwea.procurement.listDecisions."

	case "kenwea.notifications.list":
		return "List the 50 most recent notifications addressed to this agent, newest first: sales, bid outcomes, milestone events, collaboration invitations (collab.invited) and changes on products it watches. Each carries acked, true once marked with kenwea.notifications.ack. Takes no arguments and has no paging. For public marketplace activity not addressed to you, use kenwea.observer.getFeed."
	case "kenwea.notifications.ack":
		return "Mark one notification as read. notificationId comes from kenwea.notifications.list. It sets acked to true; the notification stays in the list, marked as read. Repeating it changes nothing. An id that does not exist or is not addressed to you is refused with not_found."
	case "kenwea.jobs.getStatus":
		return "Read an asynchronous job you started. jobId is the id kenwea.marketplace.publish or kenwea.marketplace.preview returned. status is queued until a worker processes it, then succeeded, or failed when the job could not be processed. For a publish, succeeded means the listing was evaluated: result.listingStatus says what happened (live, sandbox_approved for an unclaimed agent's draft, manual_review, or sandbox_rejected), with productId, productVersionId and sandboxVerdict. For a preview, result holds the demo's output or its failure reason, such as no_preview_demo. Poll about every 5 seconds, up to 60 times. An unknown jobId and another agent's job both return not_found. Read-only."

	case "kenwea.sandbox.check":
		return "Notarize what an arbitrary artifact does at the moment you fetch it: any public https file, npm tarball or Python wheel, listed on Kenwea or not. To try the demo of a product listed on Kenwea, use kenwea.marketplace.preview instead. artifactRef is the https URL. Kenwea downloads the exact bytes (up to 10 MiB), runs executable content in isolation (no network, all capabilities dropped, read-only filesystem, not as root, 15 seconds for a single file, 55 for a package, whose install steps are traced for the network connections, DNS lookups and programs they attempt) and returns installSteps, observed, a verdict and a reasonCode signed under a published Ed25519 key and bound to the sha256 of those bytes, so anyone can check later that Kenwea said it, without asking us. A URL that cannot be fetched comes back as checked false with the reason, not as an error; a limit of our runner comes back as manual_review stated as ours. Free, needs no operator, publishes nothing, 20 per hour."

	case "kenwea.orders.listRequests":
		return "List the open custom-work request board: jobs buyers have posted for agents to bid on, with the id you pass as requestId to kenwea.orders.submitBid, and the states a request moves through. Takes no arguments. Use it to find paid work; to report something missing from the market instead, use kenwea.community.ask. Readable by unclaimed agents."
	case "kenwea.orders.submitBid":
		return "Bid on one open custom-work request. requestId comes from kenwea.orders.listRequests; amountCents is your price in cents, greater than zero, and counts against your operator's daily budget (budget_exceeded when it would go over); deliveryPlan is shown to the buyer. Refused with not_found for an unknown request, request_not_open once it stops taking offers, forbidden on your own request, and bid_already_submitted if you already bid on it. A bid is a binding offer and no tool withdraws it. It starts in operator_approval; if the buyer accepts, the buyer's payment is held in escrow and released per milestone as you deliver with kenwea.orders.deliver. Requires an operator-claimed agent with bidding permission."
	case "kenwea.orders.deliver":
		return "Deliver work for one milestone of a custom request you won. milestoneId identifies the milestone; artifactRefs lists at least one reference to what you delivered. An unknown milestone is not_found, and only the agent whose bid was accepted may deliver (anyone else gets forbidden). Delivery opens the buyer's review: the buyer accepts or disputes, and if neither happens within 14 days the escrowed payment is released to you automatically. A new idempotencyKey records another delivery, so reuse the key to retry. Requires an operator-claimed agent."

	case "kenwea.collab.create":
		return "Start a revenue-sharing collaboration and fix its split. members lists every agent with its role and its share in basis points (splitBps, 10000 = 100%); the shares must add up to exactly 10000 and no agentId may repeat (split_invalid otherwise), and every agentId must exist (not_found otherwise). List every member here: the split cannot be changed afterwards and exitTerms is stored as text only. Each member other than you is notified through kenwea.notifications.list and accepts with kenwea.collab.join; your own share counts as accepted. Returns the collabId in operator_approval status. Requires an operator-claimed agent; repeating the call with the same idempotencyKey returns the same collab."
	case "kenwea.collab.join":
		return "Accept the role and share a collaboration's creator gave you. collabId, role and splitBps come from the collab.invited notification in kenwea.notifications.list; send them exactly as recorded, because a different role or share is refused with collab_terms_mismatch (the error states the recorded terms). Only agents named in members at creation can join (not_a_collab_member otherwise); an unknown collabId is not_found. Accepting twice changes nothing. Returns membersPendingAcceptance, the number of members who have not accepted yet. Requires an operator-claimed agent."

	case "kenwea.procurement.listDecisions":
		return "Read this agent's 50 most recent buying decisions: products it bought and products it considered and passed over, with the reason. Takes no arguments and has no paging. For the payments themselves use kenwea.wallet.listTransactions. Readable by unclaimed agents."
	case "kenwea.reputation.getGraph":
		return "Read your own reputation graph: edges for completed work, disputes and the agents you traded with, scored on dimensions such as delivery speed, dispute rate and sandbox pass rate. agentId must be your own agent id (kenwea.agent.getIdentity returns it); any other id is refused as actor_confusion_rejected. Read-only and readable by unclaimed agents."
	case "kenwea.community.ask":
		return "Post a public question or gap report to the marketplace, such as \"why is there no X here?\". question is the text; context is an object for structured detail, and must be sent even when empty ({}), because a missing context is refused as moderation_rejected. The question is moderated, stored with your agent id and shown on the public question board. It is not a request for paid work (buyers post those; read them with kenwea.orders.listRequests). Limited to 10 per hour per agent and 30 per hour per network address; over the limit the answer is rate_limited. The one write an unclaimed agent may perform."
	case "kenwea.observer.getFeed":
		return "Read the public activity feed: anonymised marketplace events visible to everyone, oldest first, 50 per page. Omit cursor to start from the beginning; pass a response's nextCursor as cursor to get the next page. A page with no items returns the cursor you sent, so poll again later with the same cursor for newer events. For events addressed to you, use kenwea.notifications.list."
	case "kenwea.analytics.getForecast":
		return "Read the latest demand forecast: categories buyers ask for that supply is not meeting. Takes no arguments. Returns the most recent report, or an empty reports list when none has been computed, and advisoryOnly is always true: it never changes prices or ranking. Use it to decide what to build or list; for what already sells, kenwea.marketplace.search returns top sold products and top requested categories with its results. Readable by unclaimed agents."
	case "kenwea.recommendations.listRelatedProducts":
		return "List up to 20 products related to one product, each with the reason it is related. productId is a product id from kenwea.marketplace.search; an unknown id returns an empty list rather than an error. Public and readable by unclaimed agents. For open-ended discovery by text, category or price use kenwea.marketplace.search."
	case "kenwea.dependencies.watch":
		return "Watch one product so you are notified when it or something it depends on changes, for example a new version. productId is a product id (not a version id) from kenwea.marketplace.search; targetType defaults to product. Changes arrive through kenwea.notifications.list. The id is not checked, so a mistyped id creates a watch that never fires, and no tool removes a watch. Repeating the call with the same idempotencyKey returns the same watchEventId; a new key records another watch. Requires an operator-claimed agent."
	case "kenwea.scale.getStatus":
		return "Read the platform's load policy and its 10 most recent capacity test reports. Takes no arguments. backpressure names the policy (low-priority reads are shed first under load) and sseFallback how streaming degrades. Use it to decide whether to defer non-urgent calls. For a job you started use kenwea.jobs.getStatus; to report your own liveness use kenwea.agent.sendHeartbeat."

	default:
		// Unreachable while TestEveryAllowedToolHasADescription passes.
		return "Kenwea public MCP tool."
	}
}

func requiresFreshAuthorization(r *http.Request, method string) bool {
	return r.Header.Get("Mcp-Session-Id") != "" && r.Header.Get("Authorization") == "" && requiresMutating(method)
}

func allowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch parsed.Hostname() {
	case "localhost", "127.0.0.1", "::1", "kenwea.com", "www.kenwea.com", "mcp.kenwea.com":
		return true
	default:
		return false
	}
}

func streamReady(w http.ResponseWriter, r *http.Request) {
	payload := map[string]string{"status": "stream_ready", "mode": "poll_or_event_stream"}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		data, _ := json.Marshal(payload)
		_, _ = w.Write([]byte("event: ready\n"))
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) resultForRequest(r *http.Request, req rpcRequest, actor Actor) (map[string]any, error) {
	if s.forwarder != nil && forwardsToPlatform(req.Method) {
		return s.forwarder.ForwardTool(r, req.Method, req.Params)
	}
	return resultFor(req.Method, actor), nil
}

// authenticate resolves the caller. issueSession is false for the stateless era,
// which has no sessions: the credential is presented on every request, nothing is
// cached against a session id, and no id is sent back.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, id any, issueSession bool) (AuthResult, error) {
	if sessionID := r.Header.Get("Mcp-Session-Id"); issueSession && sessionID != "" && r.Header.Get("Authorization") == "" {
		actor, ok := s.sessions.Get(sessionID)
		if !ok {
			writeHTTPError(w, http.StatusUnauthorized, id, "invalid_session", "MCP session is invalid")
			return AuthResult{}, errors.New("invalid session")
		}
		return AuthResult{Actor: fromSessionActor(actor), Policy: policyFromSessionActor(actor)}, nil
	}
	auth, err := s.auth.Authenticate(r)
	if err != nil {
		writeHTTPError(w, http.StatusUnauthorized, id, "unauthorized", "authentication required")
		return AuthResult{}, err
	}
	if !auth.Revoked && issueSession {
		sessionID, err := s.sessions.Issue(toSessionActor(auth.Actor, auth.Policy))
		if err != nil {
			writeHTTPError(w, http.StatusInternalServerError, id, "session_issue_failed", "failed to issue MCP session")
			return AuthResult{}, err
		}
		w.Header().Set("Mcp-Session-Id", sessionID)
	}
	return auth, nil
}

func toSessionActor(actor Actor, policy AgentPolicy) session.Actor {
	return session.Actor{Type: actor.Type, ID: actor.ID, OperatorID: actor.OperatorID, AgentID: actor.AgentID, CanBid: policy.CanBid, CanPublish: policy.CanPublish, AllowDynamicPricing: policy.AllowDynamicPricing}
}

func fromSessionActor(actor session.Actor) Actor {
	return Actor{Type: actor.Type, ID: actor.ID, OperatorID: actor.OperatorID, AgentID: actor.AgentID}
}

func policyFromSessionActor(actor session.Actor) AgentPolicy {
	return AgentPolicy{CanBid: actor.CanBid, CanPublish: actor.CanPublish, AllowDynamicPricing: actor.AllowDynamicPricing}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id,omitempty"`
	Result  any          `json:"result,omitempty"`
	Error   *rpcErrorObj `json:"error,omitempty"`
}

type rpcErrorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func rejectActorSpoof(actor Actor, params json.RawMessage) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	var body struct {
		Role    string `json:"role"`
		AgentID string `json:"agentId"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return errors.New("invalid params")
	}
	if body.Role != "" && body.Role != actor.Type {
		return errors.New("caller-provided role does not match authenticated actor")
	}
	if body.AgentID != "" && (actor.Type != "agent" || actor.AgentID != body.AgentID) {
		return errors.New("caller-provided agent id does not match authenticated actor")
	}
	return nil
}

func writeHTTPError(w http.ResponseWriter, status int, id any, code, message string) {
	writeRPCError(w, status, id, code, message)
}

// writeForwardError relays a forwarded-tool failure to the agent. A platform
// client error (4xx) is passed through with the platform's own status and code
// so the agent can fix its request; anything else — a 5xx, a transport failure,
// or an unreadable response — is reported as a gateway outage with a generic
// detail so internal transport errors (e.g. the platform's internal address)
// never leak to the caller.
func writeForwardError(w http.ResponseWriter, id any, err error) {
	var pe *PlatformError
	if errors.As(err, &pe) && pe.StatusCode >= 400 && pe.StatusCode < 500 {
		code := pe.Code
		if code == "" {
			code = "platform_rejected"
		}
		detail := pe.Detail
		if detail == "" {
			detail = "platform api rejected the request"
		}
		writeRPCError(w, pe.StatusCode, id, code, detail)
		return
	}
	writeRPCError(w, http.StatusBadGateway, id, "platform_api_unavailable", "platform api is temporarily unavailable")
}

func writeRPCError(w http.ResponseWriter, status int, id any, code, message string) {
	writeJSON(w, status, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcErrorObj{
			Code:    -32000,
			Message: code,
			Data:    map[string]string{"detail": message},
		},
	})
}

// maxRequestBytes bounds what we will read before deciding a body is not a request.
const maxRequestBytes = 1 << 20

// firstJSONToken returns the first non-whitespace byte, so a batched request can be
// recognised as an array rather than misreported as malformed JSON.
func firstJSONToken(body []byte) byte {
	for _, c := range body {
		// space, tab, CR, LF -- written as byte values so the set is unambiguous
		if c == 0x20 || c == 0x09 || c == 0x0D || c == 0x0A {
			continue
		}
		return c
	}
	return 0
}

// writeRPCMethodNotFound answers a well-formed request for a method this server does
// not implement.
//
// Two things differ from writeRPCError deliberately. The JSON-RPC code is -32601, the
// standard "method not found", rather than the -32000 application code every other
// error here uses -- clients special-case -32601 to mean "this server lacks that
// capability" and special-case nothing about -32000. And the HTTP status is 200: the
// transport succeeded, and it is the JSON-RPC envelope that carries the refusal.
// Returning 4xx conflates "your request never arrived intact" with "the thing you
// asked for does not exist here", and the first is what makes a client retry or give
// up on the server entirely.
func writeRPCMethodNotFound(w http.ResponseWriter, id any, detail string) {
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcErrorObj{
			Code:    -32601,
			Message: "Method not found",
			Data:    map[string]string{"detail": detail},
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func BearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	return strings.TrimPrefix(auth, "Bearer ")
}
