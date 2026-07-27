package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The tier label is what makes the funnel legible: a registered-but-unclaimed
// agent is a tourist, and "tourists actually browsing" is the signal that the
// no-commitment path works.
func TestActorTierClassifiesTouristVersusBound(t *testing.T) {
	cases := []struct {
		name  string
		actor Actor
		want  string
	}{
		{"unbound agent is a tourist", Actor{Type: "agent", ID: "agent_1"}, "tourist"},
		{"operator-claimed agent is bound", Actor{Type: "agent", ID: "agent_1", OperatorID: "op_1"}, "operator_bound"},
		{"empty actor is anonymous", Actor{}, "anonymous"},
		{"non-agent actor keeps its own type", Actor{Type: "operator", ID: "op_1"}, "operator"},
	}
	for _, tc := range cases {
		if got := actorTier(tc.actor); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMetricsExposesToolAndTierCounters(t *testing.T) {
	counters := &usageCounters{byTool: map[string]uint64{}, byTier: map[string]uint64{}}
	original := toolUsage
	toolUsage = counters
	defer func() { toolUsage = original }()

	counters.record("kenwea.marketplace.search", "tourist")
	counters.record("kenwea.marketplace.search", "tourist")
	counters.record("kenwea.orders.listRequests", "tourist")
	counters.record("kenwea.marketplace.publish", "operator_bound")
	counters.recordRegistration()

	rec := httptest.NewRecorder()
	writeMCPMetrics(rec)
	body := rec.Body.String()

	for _, want := range []string{
		`kenwea_mcp_tool_calls_total{tool="kenwea.marketplace.search"} 2`,
		`kenwea_mcp_tool_calls_total{tool="kenwea.orders.listRequests"} 1`,
		`kenwea_mcp_tool_calls_by_tier_total{tier="tourist"} 3`,
		`kenwea_mcp_tool_calls_by_tier_total{tier="operator_bound"} 1`,
		"kenwea_mcp_self_registrations_total 1",
		// The pre-existing liveness gauge must survive, since the Prometheus
		// alert rule KenweaMCPBackpressureHigh watches this target.
		"kenwea_mcp_up 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n--- got ---\n%s", want, body)
		}
	}
}

// Telemetry must never carry call parameters: a search query or product id
// reveals buyer intent, and this is instrumentation, not surveillance.
func TestMetricsNeverLeaksCallParameters(t *testing.T) {
	counters := &usageCounters{byTool: map[string]uint64{}, byTier: map[string]uint64{}}
	original := toolUsage
	toolUsage = counters
	defer func() { toolUsage = original }()

	// Only a tool name is ever passed in; this asserts the recording surface
	// itself has no place to put a parameter.
	counters.record("kenwea.marketplace.search", "tourist")
	rec := httptest.NewRecorder()
	writeMCPMetrics(rec)

	// Guards against leaking VALUES, not the word "token": the proposal funnel
	// metric names legitimately contain "tokens" while carrying only counts, so a
	// bare substring check on that word is a false positive. `kwp1.` is the actual
	// attribution-token prefix, which must never appear in an exported series.
	for _, forbidden := range []string{"query=", "productId", "agent_", "kw_agent", "secret=", "kwp1."} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("metrics output leaked %q", forbidden)
		}
	}
}

// A served tool call must actually increment the counters end to end through the
// real handler, otherwise the instrumentation is decorative.
func TestServedToolCallIncrementsCounters(t *testing.T) {
	counters := &usageCounters{byTool: map[string]uint64{}, byTier: map[string]uint64{}}
	original := toolUsage
	toolUsage = counters
	defer func() { toolUsage = original }()

	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.search","params":{"query":"prompt"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("tool call failed: %d %s", rec.Code, rec.Body.String())
	}
	tools, tiers, _ := counters.snapshot()
	if len(tools) != 1 || tools[0].Label != "kenwea.marketplace.search" || tools[0].Count != 1 {
		t.Fatalf("tool counter not incremented: %+v", tools)
	}
	// An unbound agent browsing is exactly the tourist signal this exists for.
	if len(tiers) != 1 || tiers[0].Label != "tourist" || tiers[0].Count != 1 {
		t.Fatalf("tier counter wrong: %+v", tiers)
	}
}

// Rejected calls must not be counted as usage, or the funnel numbers become
// meaningless (a wall of operator_required denials would read as engagement).
func TestRejectedCallIsNotCountedAsUsage(t *testing.T) {
	counters := &usageCounters{byTool: map[string]uint64{}, byTier: map[string]uint64{}}
	original := toolUsage
	toolUsage = counters
	defer func() { toolUsage = original }()

	// An unbound (tourist) agent attempting a seller-only tool is refused.
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.publish","params":{"title":"x"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected the unbound publish attempt to be refused, got 200: %s", rec.Body.String())
	}
	tools, _, _ := counters.snapshot()
	if len(tools) != 0 {
		t.Fatalf("a refused call was counted as usage: %+v", tools)
	}
}
