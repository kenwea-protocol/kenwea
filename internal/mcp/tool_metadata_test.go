package mcp

import "testing"

// Every advertised tool must carry annotations. A descriptor that silently drops
// them for one tool is worse than publishing none at all: a client reading the
// list has no way to tell "no hint given" from "hint forgotten", and will treat
// the missing tool as the conservative default when we may have meant otherwise.
func TestEveryToolCarriesAnnotationsAndATitle(t *testing.T) {
	for _, descriptor := range mcpToolDescriptors() {
		name, _ := descriptor["name"].(string)
		annotations, ok := descriptor["annotations"].(map[string]any)
		if !ok {
			t.Errorf("%s has no annotations", name)
			continue
		}
		for _, hint := range []string{"readOnlyHint", "idempotentHint", "openWorldHint", "destructiveHint"} {
			if _, present := annotations[hint]; !present {
				t.Errorf("%s is missing %s", name, hint)
			}
		}
		if descriptor["title"] == nil || descriptor["title"] == "" {
			t.Errorf("%s has no title", name)
		}
	}
}

// The claim that must never drift: anything the server itself classifies as a
// write cannot be advertised as read-only. This is the pairing that keeps the
// published hint tied to enforced behaviour rather than to a hand-maintained
// list -- add a tool to mutatingTools and forget the annotation, and this fails.
func TestNothingMutatingIsAdvertisedAsReadOnly(t *testing.T) {
	for name := range mutatingTools {
		annotations := toolAnnotations(name)
		if annotations["readOnlyHint"] == true {
			t.Errorf("%s is in mutatingTools but is advertised readOnlyHint=true", name)
		}
	}
}

// The permit half, so the test above cannot pass by marking everything writable.
// These are pure reads and must stay advertised as such; an agent that cannot
// tell a read from a write has to treat the whole surface as dangerous.
func TestReadsAreAdvertisedAsReadOnly(t *testing.T) {
	for _, name := range []string{
		"kenwea.marketplace.search",
		"kenwea.observer.feed",
		"kenwea.jobs.getStatus",
		"kenwea.wallet.balance",
		"kenwea.scale.status",
	} {
		if toolAnnotations(name)["readOnlyHint"] != true {
			t.Errorf("%s is a pure read and should be advertised readOnlyHint=true", name)
		}
	}
}

// heartbeat is the case that catches a lazy derivation. It is deliberately NOT in
// mutatingTools -- it is a liveness ping, not an economic action -- but it does
// write last_heartbeat_at. Anyone tempted to compute readOnlyHint as
// "not in mutatingTools" publishes a false claim here.
func TestHeartbeatIsNotClaimedReadOnlyDespiteBeingNonMutating(t *testing.T) {
	if _, mutating := mutatingTools["kenwea.agent.heartbeat"]; mutating {
		t.Fatal("premise changed: heartbeat is now in mutatingTools, so this test no longer guards anything")
	}
	if toolAnnotations("kenwea.agent.heartbeat")["readOnlyHint"] == true {
		t.Error("heartbeat writes last_heartbeat_at, so it must not be advertised as read-only")
	}
}

// idempotentHint is not idempotentTools. That set means "requires an
// Idempotency-Key" -- a caller-driven deduplication mechanism -- while the spec
// asks whether repeating the call with the same ARGUMENTS has no further effect.
// Publishing twice with two fresh keys makes two listings, so the honest answer
// there is false. If someone ever wires one set to the other, this fails.
func TestIdempotentHintIsNotCopiedFromTheIdempotencyKeySet(t *testing.T) {
	if _, keyed := idempotentTools["kenwea.marketplace.publish"]; !keyed {
		t.Fatal("premise changed: publish no longer takes an Idempotency-Key")
	}
	if toolAnnotations("kenwea.marketplace.publish")["idempotentHint"] == true {
		t.Error("publish takes an Idempotency-Key but is not idempotent by arguments; advertising otherwise invites a caller to retry into a second listing")
	}
	// And the genuinely idempotent case, so this is a distinction and not a blanket no.
	if toolAnnotations("kenwea.notifications.ack")["idempotentHint"] != true {
		t.Error("acking the same notification twice leaves the same state and should say so")
	}
}

// openWorldHint is the annotation with real safety content here: it marks the
// tools that make our servers fetch an address the CALLER chose. Exactly three
// do. Both halves matter -- a missed one understates what a call does, and a
// spurious one makes an ordinary database read look like an outbound request.
func TestOpenWorldMarksExactlyTheToolsThatFetchACallerSuppliedURL(t *testing.T) {
	fetches := map[string]bool{
		"kenwea.sandbox.check":       true,
		"kenwea.marketplace.publish": true,
		"kenwea.marketplace.preview": true,
	}
	for name := range allowedTools {
		got := toolAnnotations(name)["openWorldHint"] == true
		if got != fetches[name] {
			t.Errorf("%s: openWorldHint=%v, expected %v", name, got, fetches[name])
		}
	}
}

// purchase is the only tool that moves money out of a wallet. A caller weighing a
// speculative call has to be told that, and "additive because it mints a license"
// is not a defence a spender would accept.
func TestSpendingAndBindingToolsAreMarkedDestructive(t *testing.T) {
	for _, name := range []string{
		"kenwea.marketplace.purchase",
		"kenwea.orders.submitBid",
		"kenwea.orders.deliver",
	} {
		if toolAnnotations(name)["destructiveHint"] != true {
			t.Errorf("%s commits money or work irreversibly and must be marked destructive", name)
		}
	}
	// The complement: a check spends compute and nothing else. Marking it
	// destructive would make the one tool a stranger should feel safe trying look
	// like the one they should avoid.
	if toolAnnotations("kenwea.sandbox.check")["destructiveHint"] != false {
		t.Error("sandbox.check creates no product, version, listing or report and must not be marked destructive")
	}
}

// Declaring an outputSchema is a promise to return conforming structuredContent.
// A closed schema turns the platform's next additive field into a conformance
// failure on our side, which is a promise we would break by standing still.
func TestOutputSchemasStayOpenToNewFields(t *testing.T) {
	for _, descriptor := range mcpToolDescriptors() {
		schema, ok := descriptor["outputSchema"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := descriptor["name"].(string)
		if schema["type"] != "object" {
			t.Errorf("%s: outputSchema must be an object schema", name)
		}
		if closed, present := schema["additionalProperties"]; present && closed == false {
			t.Errorf("%s: outputSchema is closed; the platform adding a field would break conformance", name)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok || len(properties) == 0 {
			t.Errorf("%s: outputSchema declares no properties, which promises nothing", name)
		}
	}
}

// Coverage is now complete, and this test flipped to keep meaning something.
//
// It used to fail if EVERY tool had a schema, because at the time most shapes had
// not been captured and a full sweep would have meant somebody guessed. On
// 2026-08-06 the remaining sixteen were pinned down properly -- three captured live
// from production, thirteen read out of the handler or store function that builds
// the response -- so the old assertion was asserting the wrong thing.
//
// The invariant that replaces it is stronger: a NEW tool cannot ship without a
// schema. That forces whoever adds one to go and establish its real shape, which is
// exactly the work the old test was protecting.
func TestEveryToolDeclaresAnOutputSchema(t *testing.T) {
	for _, descriptor := range mcpToolDescriptors() {
		if descriptor["outputSchema"] == nil {
			name, _ := descriptor["name"].(string)
			t.Errorf("%s declares no outputSchema. Capture its real shape -- call it against production, or read the handler that builds the response -- and add it. Do not infer one from the tool name: the schema is a promise to return conforming structuredContent.", name)
		}
	}
}
