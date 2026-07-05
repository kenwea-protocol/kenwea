package mcp

import (
	"encoding/json"
	"errors"
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
)

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

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	version := r.Header.Get("MCP-Protocol-Version")
	if version != ProtocolCurrent && version != ProtocolCompat {
		writeHTTPError(w, http.StatusBadRequest, nil, "unsupported_protocol_version", "unsupported MCP protocol version")
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, nil, "invalid_json", "invalid JSON-RPC request")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, "invalid_request", "invalid JSON-RPC request")
		return
	}
	if s.handleMCPProtocolMethod(w, r, req) {
		return
	}
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
		writeRPCError(w, http.StatusBadRequest, req.ID, "method_not_allowed", "tool is outside active public MCP scope")
		return
	}
	if requiresFreshAuthorization(r, req.Method) {
		writeRPCError(w, http.StatusUnauthorized, req.ID, "authorization_required", "mutating MCP tools require fresh Authorization so revoked keys cannot continue through a cached session")
		return
	}
	if req.Method == "kenwea.onboarding.registerSelf" {
		result, err := s.resultForRequest(r, req, Actor{})
		if err != nil {
			writeRPCError(w, http.StatusBadGateway, req.ID, "platform_api_unavailable", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: responseResult(result, wrapToolResult)})
		return
	}
	auth, err := s.authenticate(w, r)
	if err != nil {
		return
	}
	if auth.Revoked {
		writeHTTPError(w, http.StatusUnauthorized, nil, "revoked_key", "agent key has been revoked")
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
	if requiresIdempotency(req.Method) && idempotencyKey(r) == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, "idempotency_required", "financial or destructive MCP tool requires Idempotency-Key")
		return
	}
	if err := validateToolParams(req.Method, req.Params); err != nil {
		writeRPCError(w, http.StatusBadRequest, req.ID, "validation_failed", err.Error())
		return
	}
	if err := enforceOperatorPolicy(auth.Policy, req.Method, req.Params); err != nil {
		writeRPCError(w, http.StatusForbidden, req.ID, policyCode(err), err.Error())
		return
	}
	result, err := s.resultForRequest(r, req, auth.Actor)
	if err != nil {
		writeRPCError(w, http.StatusBadGateway, req.ID, "platform_api_unavailable", err.Error())
		return
	}
	if key := idempotencyKey(r); key != "" {
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
		Result:  responseResult(result, wrapToolResult),
	})
}

func (s *Server) handleMCPProtocolMethod(w http.ResponseWriter, r *http.Request, req rpcRequest) bool {
	switch req.Method {
	case "initialize":
		writeJSON(w, http.StatusOK, rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": ProtocolCurrent,
				"capabilities": map[string]any{
					"tools": map[string]any{"listChanged": false},
				},
				"serverInfo": map[string]string{
					"name":    "kenwea-public-mcp",
					"version": "1.0.0",
				},
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
		tools = append(tools, map[string]any{
			"name":        name,
			"description": toolDescription(name),
			"inputSchema": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
		})
	}
	return tools
}

func toolDescription(name string) string {
	switch name {
	case "kenwea.onboarding.registerSelf":
		return "Self-register an unbound tourist agent and receive a one-time API key plus pairing PIN."
	case "kenwea.auth.identify", "kenwea.auth.profile", "kenwea.agent.identity":
		return "Read the authenticated Kenwea MCP actor identity."
	case "kenwea.agent.heartbeat":
		return "Send a lightweight agent heartbeat."
	case "kenwea.marketplace.search":
		return "Search public Kenwea marketplace discovery data."
	case "kenwea.orders.listRequests":
		return "List public custom request board entries visible to tourist agents."
	case "kenwea.marketplace.publish", "kenwea.orders.submitBid":
		return "Claim-gated seller action requiring Operator permissions."
	default:
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

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (AuthResult, error) {
	if sessionID := r.Header.Get("Mcp-Session-Id"); sessionID != "" && r.Header.Get("Authorization") == "" {
		actor, ok := s.sessions.Get(sessionID)
		if !ok {
			writeHTTPError(w, http.StatusUnauthorized, nil, "invalid_session", "MCP session is invalid")
			return AuthResult{}, errors.New("invalid session")
		}
		return AuthResult{Actor: fromSessionActor(actor), Policy: policyFromSessionActor(actor)}, nil
	}
	auth, err := s.auth.Authenticate(r)
	if err != nil {
		writeHTTPError(w, http.StatusUnauthorized, nil, "unauthorized", "authentication required")
		return AuthResult{}, err
	}
	if !auth.Revoked {
		sessionID, err := s.sessions.Issue(toSessionActor(auth.Actor, auth.Policy))
		if err != nil {
			writeHTTPError(w, http.StatusInternalServerError, nil, "session_issue_failed", "failed to issue MCP session")
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func BearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	return strings.TrimPrefix(auth, "Bearer ")
}
