package mcp

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// An unbound (tourist) agent: it holds a self-issued key but has not been
// claimed by an operator yet.
func touristActor() Actor {
	return Actor{Type: "agent", OperatorID: ""}
}

// community.ask is the newcomer's only way to report what the market is
// missing. If this ever regresses, a visiting agent can still browse but goes
// silent, which defeats the reason the door is open at all.
func TestTouristCanAskWhatIsMissing(t *testing.T) {
	if err := rejectUnboundMutatingAgent(touristActor(), "kenwea.community.ask"); err != nil {
		t.Fatalf("tourist must be able to call kenwea.community.ask, got: %v", err)
	}
}

// The complement, so the test above cannot pass merely because the gate was
// removed. Selling still requires an operator.
func TestTouristStillBlockedFromSellerActions(t *testing.T) {
	for _, method := range []string{
		"kenwea.marketplace.publish",
		"kenwea.marketplace.purchase",
		"kenwea.orders.submitBid",
		"kenwea.orders.deliver",
		"kenwea.wallet.balance",
	} {
		if err := rejectUnboundMutatingAgent(touristActor(), method); err == nil {
			t.Errorf("%s must still require an operator binding for an unbound agent", method)
		}
	}
}

// A bound agent is not subject to the tourist allowlist at all.
func TestBoundAgentBypassesTouristGate(t *testing.T) {
	bound := Actor{Type: "agent", OperatorID: "op_1"}
	if err := rejectUnboundMutatingAgent(bound, "kenwea.marketplace.publish"); err != nil {
		t.Fatalf("operator-bound agent should not hit the tourist gate: %v", err)
	}
}

// The README publishes this list to agent authors, so a code change that does
// not update the docs ships a lie. Compare the two directly rather than
// trusting that whoever edits one remembers the other.
func TestReadmeTouristListMatchesCode(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	section := string(raw)
	start := strings.Index(section, "Tourist-allowed tools:")
	if start < 0 {
		t.Fatal(`README is missing the "Tourist-allowed tools:" section`)
	}
	section = section[start:]
	if end := strings.Index(section, "Any other mutating action"); end > 0 {
		section = section[:end]
	}

	documented := regexp.MustCompile("`(kenwea\\.[a-zA-Z.]+)`").FindAllStringSubmatch(section, -1)
	var fromDocs []string
	for _, match := range documented {
		fromDocs = append(fromDocs, match[1])
	}
	if len(fromDocs) == 0 {
		t.Fatal("README tourist section listed no tools")
	}

	for _, method := range fromDocs {
		if !touristAllowedTool(method) {
			t.Errorf("README lists %s as tourist-allowed but the code rejects it", method)
		}
	}

	// And the other direction: nothing silently allowed but undocumented.
	sort.Strings(fromDocs)
	for _, descriptor := range mcpToolDescriptors() {
		method, _ := descriptor["name"].(string)
		if method == "" || !touristAllowedTool(method) {
			continue
		}
		idx := sort.SearchStrings(fromDocs, method)
		if idx >= len(fromDocs) || fromDocs[idx] != method {
			t.Errorf("%s is tourist-allowed in code but missing from the README list", method)
		}
	}
}
