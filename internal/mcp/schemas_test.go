package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	idempotencytest "github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/idempotency/teststore"
	sessiontest "github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/session/teststore"
)

// A schema is a promise about how to call a tool. These tests exist because the
// previous promise -- one blanket `{"type":"object","additionalProperties":true}` for
// all 29 tools -- was true and useless, and nothing failed while it was there. Each
// test below fails on a specific way a schema can drift back into being decoration.

func TestEveryAllowedToolHasASchema(t *testing.T) {
	for name := range allowedTools {
		if _, _, known := toolParameters(name); !known {
			t.Errorf("tool %q is exposed in tools/list with no declared schema; an agent would have to guess its arguments", name)
		}
	}
}

func TestEveryAllowedToolHasADescription(t *testing.T) {
	const placeholder = "Kenwea public MCP tool."
	for name := range allowedTools {
		if description := toolDescription(name); description == placeholder {
			t.Errorf("tool %q falls through to the placeholder description, which names the vendor and not the tool", name)
		}
	}
}

// Required fields must exist in properties. A required name with no property is a
// schema that demands an argument it never describes -- exactly the failure mode this
// whole file replaces, just expressed one level down.
func TestRequiredFieldsAreDeclaredAsProperties(t *testing.T) {
	for name := range allowedTools {
		schema := toolInputSchema(name)
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]string)
		for _, field := range required {
			if _, ok := properties[field]; !ok {
				t.Errorf("tool %q requires %q but does not declare it as a property", name, field)
			}
		}
	}
}

// Every property carries a description. A bare {"type":"string"} tells an agent the
// argument exists and nothing about what belongs in it.
func TestEveryPropertyIsDescribed(t *testing.T) {
	for name := range allowedTools {
		schema := toolInputSchema(name)
		properties, _ := schema["properties"].(map[string]any)
		for field, raw := range properties {
			property, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("tool %q property %q is not an object", name, field)
				continue
			}
			if description, _ := property["description"].(string); strings.TrimSpace(description) == "" {
				t.Errorf("tool %q property %q has no description", name, field)
			}
		}
	}
}

// The measured defect: registerSelf is the first call any agent makes, and the obvious
// guess `name` is silently dropped and reported back as a missing agent name. The
// schema must name the field the server actually reads.
func TestRegisterSelfSchemaNamesTheFieldTheServerValidates(t *testing.T) {
	schema := toolInputSchema("kenwea.onboarding.registerSelf")
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["agentName"]; !ok {
		t.Fatal("registerSelf schema does not declare agentName, the field the platform validates")
	}
	if _, ok := properties["name"]; ok {
		t.Fatal("registerSelf schema declares `name`, which the platform ignores")
	}
	required, _ := schema["required"].([]string)
	if !schemaListContains(required, "agentName") {
		t.Fatalf("agentName must be required; got %v", required)
	}
}

// The publish enum is a copy of the validator's allowlist. A copy that can drift is
// worse than no copy: it would show a seller a category that is then rejected.
func TestPublishSchemaCategoriesMatchValidator(t *testing.T) {
	for _, category := range publishCategories {
		if !allowedMarketplaceCategory(category) {
			t.Errorf("schema advertises category %q which allowedMarketplaceCategory rejects", category)
		}
	}
	// The other direction: a category the validator accepts but the schema omits is
	// invisible to a seller reading tools/list. Counted rather than enumerated,
	// because the validator's set is a switch and cannot be ranged over.
	if len(publishCategories) != 29 {
		t.Errorf("publishCategories has %d entries; allowedMarketplaceCategory lists 29. Update both together.", len(publishCategories))
	}
}

// Publish is the tool an agent must get right to earn anything. Its six mandatory
// fields are asserted by name so a silent removal fails here rather than in a seller's
// first attempt.
func TestPublishSchemaDeclaresEveryMandatoryField(t *testing.T) {
	schema := toolInputSchema("kenwea.marketplace.publish")
	required, _ := schema["required"].([]string)
	for _, field := range []string{"title", "version", "summary", "category", "license", "artifactRef", "sellerAgreementAccepted", "images"} {
		if !schemaListContains(required, field) {
			t.Errorf("publish schema does not mark %q required, but the platform rejects a publish without it", field)
		}
	}
}

// Every tool behind the idempotency gate must advertise the argument that satisfies it,
// or the schema describes a call the server will refuse.
func TestIdempotentToolsAdvertiseTheIdempotencyKey(t *testing.T) {
	for name := range idempotentTools {
		schema := toolInputSchema(name)
		properties, _ := schema["properties"].(map[string]any)
		if _, ok := properties["idempotencyKey"]; !ok {
			t.Errorf("tool %q is gated by requiresIdempotency but its schema does not declare idempotencyKey", name)
		}
		required, _ := schema["required"].([]string)
		if !schemaListContains(required, "idempotencyKey") {
			t.Errorf("tool %q must mark idempotencyKey required; the gate returns idempotency_required without it", name)
		}
	}
	// And the converse: a tool that is not gated must not demand the key.
	for name := range allowedTools {
		if _, gated := idempotentTools[name]; gated {
			continue
		}
		properties, _ := toolInputSchema(name)["properties"].(map[string]any)
		if _, ok := properties["idempotencyKey"]; ok {
			t.Errorf("tool %q advertises idempotencyKey but is not behind the idempotency gate", name)
		}
	}
}

// The whole point of the schemas is that they survive a JSON round trip into a client.
func TestToolsListSerializesWithRealSchemas(t *testing.T) {
	payload, err := json.Marshal(mcpToolDescriptors())
	if err != nil {
		t.Fatalf("tool descriptors do not marshal: %v", err)
	}
	var tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(payload, &tools); err != nil {
		t.Fatalf("tool descriptors do not round trip: %v", err)
	}
	if len(tools) != len(allowedTools) {
		t.Fatalf("tools/list returned %d tools, want %d", len(tools), len(allowedTools))
	}
	for _, tool := range tools {
		if _, alias := unlistedAliases[tool.Name]; alias {
			t.Fatalf("tools/list advertises %s, an unlisted alias", tool.Name)
		}
	}
	// The regression this file exists to prevent: at least one tool with declared
	// arguments, rather than 29 empty objects.
	described := 0
	for _, tool := range tools {
		if len(tool.InputSchema.Properties) > 0 {
			described++
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %q inputSchema type is %q, not object", tool.Name, tool.InputSchema.Type)
		}
	}
	if described < 15 {
		t.Fatalf("only %d of %d tools declare any argument; the surface has regressed toward the blanket schema", described, len(tools))
	}
}

// An MCP client calling tools/call cannot set an HTTP header, so before this the ten
// gated tools were unreachable from a conforming client. The key must be accepted from
// the arguments object.
func TestIdempotencyKeyIsAcceptedFromToolArguments(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kenwea.dependencies.watch","arguments":{"productId":"prod_1","idempotencyKey":"idem_from_params"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	recorder := httptest.NewRecorder()

	forwarder := &schemaTestForwarder{result: map[string]any{"status": "watching"}}
	server := NewServerWithRuntime(
		StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_1", AgentID: "agent_1", OperatorID: "op_1"}},
		sessiontest.New(), idempotencytest.New(), forwarder,
	)
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected the call to pass the idempotency gate, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !forwarder.called {
		t.Fatal("the tool was never forwarded")
	}
}

// And the gate still bites when neither form is present -- removing the header
// requirement must not have removed the requirement.
func TestIdempotencyGateStillRejectsWhenNeitherFormIsPresent(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kenwea.dependencies.watch","arguments":{"productId":"prod_1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	recorder := httptest.NewRecorder()

	server := NewServerWithRuntime(
		StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_1", AgentID: "agent_1", OperatorID: "op_1"}},
		sessiontest.New(), idempotencytest.New(), &schemaTestForwarder{result: map[string]any{}},
	)
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 idempotency_required, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "idempotency_required") {
		t.Fatalf("expected idempotency_required, got %s", recorder.Body.String())
	}
}

func TestIdempotencyKeyFromParams(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		params string
		want   string
	}{
		{"present", `{"idempotencyKey":"idem_1"}`, "idem_1"},
		{"trimmed", `{"idempotencyKey":"  idem_1  "}`, "idem_1"},
		{"absent", `{"productId":"p"}`, ""},
		{"empty params", ``, ""},
		{"null params", `null`, ""},
		{"malformed", `{`, ""},
		{"wrong type", `{"idempotencyKey":7}`, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IdempotencyKeyFromParams(json.RawMessage(testCase.params)); got != testCase.want {
				t.Fatalf("got %q, want %q", got, testCase.want)
			}
		})
	}
}

func schemaListContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type schemaTestForwarder struct {
	called bool
	result map[string]any
}

func (f *schemaTestForwarder) ForwardTool(*http.Request, string, json.RawMessage) (map[string]any, error) {
	f.called = true
	return f.result, nil
}

// The aliases leave the list but not the server: a caller that already uses an
// old name must get the same answer as before, and every alias must point at a
// tool that is itself listed, with a description distinct from every other.
func TestUnlistedAliasesStillAnswerAndListedDescriptionsAreDistinct(t *testing.T) {
	listed := map[string]bool{}
	seen := map[string]string{}
	for _, tool := range mcpToolDescriptors() {
		name := tool["name"].(string)
		listed[name] = true
		desc := tool["description"].(string)
		if other, dup := seen[desc]; dup {
			t.Fatalf("%s and %s share a description; an agent cannot tell them apart", other, name)
		}
		seen[desc] = name
	}
	for alias, target := range unlistedAliases {
		if !allowedTool(alias) {
			t.Fatalf("%s is no longer accepted; existing callers would break", alias)
		}
		if listed[alias] {
			t.Fatalf("%s is an unlisted alias but tools/list offers it", alias)
		}
		if !listed[target] {
			t.Fatalf("%s points at %s, which is not listed", alias, target)
		}
		if got := canonicalTool(alias); got != target {
			t.Fatalf("%s resolves to %s, want %s", alias, got, target)
		}
	}
}

// Every older name, called the way an existing client calls it, reaches the same
// handler as its replacement: a forwarded tool is forwarded under the new name,
// and the identity aliases answer locally with the identity envelope. Checked
// through ServeHTTP rather than canonicalTool alone, because the promise to an
// existing caller is about the wire, not about a helper.
func TestUnlistedAliasesAnswerOverTheWire(t *testing.T) {
	for alias, target := range unlistedAliases {
		forwarder := &recordingForwarder{result: map[string]any{"status": "accepted"}}
		server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
		server.forwarder = forwarder
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"agentId":"agent_01","productId":"prod_01"}}}`, alias)
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(body))
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		req.Header.Set("Authorization", "Bearer kw_agent_test")
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("%s: status=%d body=%s", alias, rec.Code, rec.Body.String())
		}
		if forwardsToPlatform(target) {
			if forwarder.method != target {
				t.Fatalf("%s forwarded as %q, want %q", alias, forwarder.method, target)
			}
		} else if !strings.Contains(rec.Body.String(), "agent_01") {
			t.Fatalf("%s did not answer with the identity envelope: %s", alias, rec.Body.String())
		}
	}
}

// A description that points an agent at another tool must point at one that is
// listed. A rename that misses one cross-reference would otherwise send callers to
// a name tools/list does not contain.
func TestDescriptionsOnlyNameListedTools(t *testing.T) {
	mention := regexp.MustCompile(`kenwea\.[a-zA-Z]+\.[a-zA-Z]+`)
	for _, tool := range mcpToolDescriptors() {
		name := tool["name"].(string)
		for _, ref := range mention.FindAllString(tool["description"].(string), -1) {
			if _, listed := allowedTools[ref]; listed {
				continue
			}
			if _, alias := unlistedAliases[ref]; alias {
				continue
			}
			t.Errorf("%s description names %s, which is neither listed nor an alias", name, ref)
		}
	}
}
