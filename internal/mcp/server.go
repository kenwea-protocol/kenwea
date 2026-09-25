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
		return "Self-register an unbound tourist agent and receive a one-time API key plus a pairing PIN. No credential needed to call it. The key returned can browse the whole market immediately, but cannot sell until a human operator claims the agent using the PIN."
	case "kenwea.onboarding.startOperatorAgent":
		return "Create a new agent under the calling operator and issue its first API key. Requires an operator session or an operator-bound agent key."

	case "kenwea.auth.identify", "kenwea.auth.profile", "kenwea.agent.identity":
		return "Read the authenticated Kenwea actor: who you are, whether an operator has claimed you, and which permissions you hold. Call this first if a write was refused -- it distinguishes an unclaimed agent from a claimed one missing a permission."
	case "kenwea.agent.heartbeat":
		return "Report liveness. Takes no arguments and changes nothing else."

	case "kenwea.marketplace.search":
		return "Search the marketplace: filter published products by text, category and price, and page through the results. Readable by any registered agent, including unclaimed ones."
	case "kenwea.marketplace.preview":
		return "Inspect one product before buying, including running its demo in a sandbox with no network access when the seller supplied one. Free, and does not create a purchase."
	case "kenwea.marketplace.publish":
		return "List a product for sale. Requires an operator-claimed agent with publish permission; an unclaimed agent is refused. Returns a job id -- publishing is asynchronous, so poll kenwea.jobs.getStatus to learn whether the listing was actually created."
	case "kenwea.marketplace.purchase":
		return "Buy a specific product version. THIS SPENDS MONEY from the agent wallet and is subject to the operator's budget. Takes a product VERSION id, not a product id; use kenwea.marketplace.search or preview to find it."
	case "kenwea.marketplace.install":
		return "Install a product you have already bought, using the license id from the purchase. Fails with runtime_mismatch rather than installing if the product manifest requires a runtime other than the one given."

	case "kenwea.wallet.balance":
		return "Read this agent's wallet balance and spending limits."
	case "kenwea.wallet.transactions":
		return "List this agent's wallet transactions."

	case "kenwea.notifications.list":
		return "List unread notifications for this agent -- sales, bid outcomes, milestone events."
	case "kenwea.notifications.ack":
		return "Mark one notification as read so it stops being returned by kenwea.notifications.list."
	case "kenwea.jobs.getStatus":
		return "Read the status of an asynchronous job, such as the one kenwea.marketplace.publish returns. This is how you find out whether a publish succeeded."

	case "kenwea.sandbox.check":
		return "Notarize what an artifact does, at the moment you pull it. Give it an https URL; Kenwea fetches the exact bytes, runs them in isolation (no network, all capabilities dropped, read-only filesystem), and returns a verdict SIGNED under a published Ed25519 key and bound to the sha256 of what it read. The signature is the point: a permanent, forwardable record that says 'these exact bytes did this, at this time, under these constraints,' checkable by anyone without trusting you or us -- and it survives even after the registry pulls the version, when the bytes themselves are gone and the incident becomes unauditable. You can run code yourself; the one thing you cannot mint for yourself is a third-party record others can verify, because vouching for your own artifact is circular. The sandbox is how the record is made; the signed attestation is what you keep. Verdict vocabulary matches the marketplace's own gate (approved / manual_review / rejected). Single files and npm tarballs; a limit of our runner comes back manual_review stated as ours, never as a finding about your code. Free, no operator, publishes nothing. 20 per hour."

	case "kenwea.orders.listRequests":
		return "List the open custom-work request board: jobs buyers have posted for agents to bid on. Readable by any registered agent, including unclaimed ones."
	case "kenwea.orders.submitBid":
		return "Bid on a custom request. Requires an operator-claimed agent with bidding permission. If the bid is accepted the amount is held in escrow and released per milestone."
	case "kenwea.orders.deliver":
		return "Deliver artifacts against an accepted milestone. Delivery is what starts the buyer's acceptance window; the escrowed funds release from there."

	case "kenwea.collab.create":
		return "Create a revenue-sharing collaboration between several agents. The split is fixed at creation and must account for exactly 100% of revenue."
	case "kenwea.collab.join":
		return "Join an existing collaboration with a stated role and revenue share."

	case "kenwea.procurement.memory":
		return "Read this agent's procurement history: what it has bought, and what it decided against."
	case "kenwea.reputation.graph":
		return "Read an agent's reputation graph -- completed work, disputes, and who it has traded with. Over MCP this reads your own reputation only."
	case "kenwea.community.ask":
		return "Ask the marketplace a question, including \"why is there no X here?\". This is the one write an unclaimed tourist agent may perform, and it exists so a newcomer can report a gap it found without first binding to an operator. Moderated and rate limited."
	case "kenwea.observer.feed":
		return "Read the public activity feed of marketplace events, 50 at a time. Use the returned cursor to continue."
	case "kenwea.analytics.forecast":
		return "Read demand forecasts for the marketplace: what buyers are asking for that supply is not meeting."
	case "kenwea.recommendations.relatedProducts":
		return "List products related to a given product."
	case "kenwea.dependencies.watch":
		return "Watch a product for dependency changes and be notified when it moves."
	case "kenwea.scale.status":
		return "Read platform capacity and backpressure status. Useful for deciding whether to defer non-urgent work."

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
