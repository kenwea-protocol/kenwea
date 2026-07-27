package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fetchDescriptor(t *testing.T) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	writeCapabilityDescriptor(rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("descriptor returned %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("descriptor is not valid JSON: %v", err)
	}
	return body
}

// The whole point is pre-commitment evaluation: it must be reachable with no
// key, no session, and no protocol-version header.
func TestDescriptorServedWithoutAnyCredential(t *testing.T) {
	server := NewServer(StaticAuthenticator{})
	req := httptest.NewRequest(http.MethodGet, descriptorPath, nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with no credential, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if body["name"] != "com.kenwea.www/marketplace" {
		t.Errorf("wrong server name: %v", body["name"])
	}
}

// A hardcoded tool count would rot the moment a tool is added or removed. It has
// to be derived from the live tool table, so this asserts they agree.
func TestDescriptorToolCountCannotDriftFromRealToolList(t *testing.T) {
	body := fetchDescriptor(t)
	capabilities, ok := body["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("missing capabilities block")
	}
	got, ok := capabilities["toolCount"].(float64)
	if !ok {
		t.Fatalf("toolCount is not a number: %T", capabilities["toolCount"])
	}
	want := len(mcpToolDescriptors())
	if int(got) != want {
		t.Fatalf("descriptor advertises %d tools, server actually exposes %d", int(got), want)
	}
	tools, ok := capabilities["tools"].([]any)
	if !ok || len(tools) != want {
		t.Fatalf("tools list length %d does not match real tool count %d", len(tools), want)
	}
}

// The published tourist allowlist must be generated from the enforced one, or
// the descriptor would promise access the server rejects (or hide access it
// grants). Both directions are checked.
func TestDescriptorTouristTiersMatchEnforcement(t *testing.T) {
	body := fetchDescriptor(t)
	access, ok := body["accessModel"].(map[string]any)
	if !ok {
		t.Fatal("missing accessModel block")
	}
	tourist, _ := access["tourist"].(map[string]any)
	bound, _ := access["operatorBound"].(map[string]any)
	touristList, _ := tourist["allows"].([]any)
	boundList, _ := bound["allows"].([]any)

	if len(touristList) == 0 {
		t.Fatal("tourist tier advertises no tools")
	}
	for _, entry := range touristList {
		name, _ := entry.(string)
		if !touristAllowedTool(name) {
			t.Errorf("descriptor advertises %q as tourist-accessible but the gate rejects it", name)
		}
	}
	for _, entry := range boundList {
		name, _ := entry.(string)
		if touristAllowedTool(name) {
			t.Errorf("descriptor lists %q as operator-bound but the gate allows tourists", name)
		}
	}
	// Every real tool must appear in exactly one tier -- no tool silently
	// omitted from the published access model.
	if len(touristList)+len(boundList) != len(mcpToolDescriptors()) {
		t.Fatalf("tiers cover %d tools but %d exist", len(touristList)+len(boundList), len(mcpToolDescriptors()))
	}
}

// A stale embedded count is exactly the failure this descriptor is designed to
// avoid, so it must point at live endpoints for database-backed numbers instead
// of baking them in.
func TestDescriptorDoesNotEmbedStaleTrackRecordNumbers(t *testing.T) {
	body := fetchDescriptor(t)
	record, ok := body["trackRecord"].(map[string]any)
	if !ok {
		t.Fatal("missing trackRecord block")
	}
	for _, forbidden := range []string{"liveListings", "listingCount", "externalBuyers", "buyerCount", "salesCount", "totalRevenue"} {
		if _, present := record[forbidden]; present {
			t.Errorf("trackRecord embeds %q; database-backed counts must be fetched live, not frozen into the descriptor", forbidden)
		}
	}
	endpoints, ok := record["liveDataEndpoints"].(map[string]any)
	if !ok || len(endpoints) == 0 {
		t.Fatal("trackRecord must point at live data endpoints")
	}
	if !strings.Contains(record["honestState"].(string), "no external buyers") {
		t.Error("honestState must state the day-zero reality plainly")
	}
}

// The install-proposal block is the actual deliverable for the agent -> operator
// escalation, so its load-bearing fields must be present and answer the four
// questions an operator asks: what, why, what does it cost, what can it reach.
func TestDescriptorInstallProposalIsComplete(t *testing.T) {
	body := fetchDescriptor(t)
	proposal, ok := body["installProposal"].(map[string]any)
	if !ok {
		t.Fatal("missing installProposal block")
	}
	for _, field := range []string{"summary", "whyAddIt", "whatYouGate", "riskNotes", "cost", "clientConfig", "verifyBeforeTrusting"} {
		if value, present := proposal[field]; !present || value == nil {
			t.Errorf("installProposal is missing %q", field)
		}
	}
	// The pasteable config must be the real published bridge, not a placeholder.
	raw, _ := json.Marshal(proposal["clientConfig"])
	if !strings.Contains(string(raw), "@kenwea/mcp") {
		t.Errorf("clientConfig does not reference the published npm bridge: %s", raw)
	}
}

// These are enforced code boundaries; if someone deletes them from the
// descriptor the honesty claim quietly weakens, so pin the load-bearing ones.
func TestDescriptorStatesTheRealInvariants(t *testing.T) {
	body := fetchDescriptor(t)
	raw, _ := json.Marshal(body["neverDoes"])
	text := strings.ToLower(string(raw))
	for _, must := range []string{"sandbox", "declaredmodel", "tourist", "manual_review"} {
		if !strings.Contains(text, must) {
			t.Errorf("neverDoes no longer mentions %q", must)
		}
	}
}

// A consumer needs to distinguish "my cached copy is from an older build" from
// "the terms genuinely changed", so both stamps must be present.
func TestDescriptorCarriesBuildAndTermsStamps(t *testing.T) {
	body := fetchDescriptor(t)
	if body["buildVersion"] == nil || body["buildVersion"] == "" {
		t.Error("descriptor must carry a buildVersion")
	}
	fingerprint, ok := body["termsFingerprint"].(string)
	if !ok || len(fingerprint) < 8 {
		t.Fatalf("descriptor must carry a termsFingerprint, got %v", body["termsFingerprint"])
	}
}

// An unset build version must admit it rather than inventing one.
func TestBuildStampAdmitsWhenUnset(t *testing.T) {
	t.Setenv("KENWEA_BUILD_VERSION", "")
	if got := buildStamp(); got != "unstamped" {
		t.Errorf("expected an honest placeholder, got %q", got)
	}
	t.Setenv("KENWEA_BUILD_VERSION", "abc1234")
	if got := buildStamp(); got != "abc1234" {
		t.Errorf("expected the configured version, got %q", got)
	}
}

// The fingerprint must be stable for identical terms and must MOVE when any
// published term changes -- otherwise caching against it would silently serve
// stale promises.
func TestTermsFingerprintChangesOnlyWhenTermsChange(t *testing.T) {
	tools := []string{"a", "b"}
	tourist := []string{"a"}
	never := []string{"never x"}

	base := termsFingerprint(tools, tourist, 1200, never)
	if base != termsFingerprint(tools, tourist, 1200, never) {
		t.Fatal("fingerprint is not deterministic")
	}
	for name, got := range map[string]string{
		"tool added":         termsFingerprint([]string{"a", "b", "c"}, tourist, 1200, never),
		"tier changed":       termsFingerprint(tools, []string{"a", "b"}, 1200, never),
		"commission changed": termsFingerprint(tools, tourist, 1500, never),
		"guarantee changed":  termsFingerprint(tools, tourist, 1200, []string{"never y"}),
	} {
		if got == base {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}
}

// The privacy guarantee is only credible if it is actually published.
func TestDescriptorPublishesTheNoReadAttributionGuarantee(t *testing.T) {
	body := fetchDescriptor(t)
	raw, _ := json.Marshal(body["neverDoes"])
	text := strings.ToLower(string(raw))
	for _, must := range []string{"which agent read what", "aggregate", "never attributed"} {
		if !strings.Contains(text, must) {
			t.Errorf("neverDoes must state the read-attribution guarantee (missing %q)", must)
		}
	}
}
