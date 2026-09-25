package mcp

import "testing"

// The permit half. kenwea.sandbox.check exists precisely for the caller that has
// no operator -- an agent evaluating whether this server is worth binding to.
// If the gate ever closes on it the tool still appears in the list and still
// answers operator_required, which is the worst of both: advertised and
// unreachable, exactly the state kenwea.jobs.getStatus was found in on
// 2026-07-31.
func TestTouristCanCheckAnArtifact(t *testing.T) {
	if err := rejectUnboundMutatingAgent(touristActor(), "kenwea.sandbox.check"); err != nil {
		t.Fatalf("an unbound agent must be able to call kenwea.sandbox.check, got: %v", err)
	}
}

// The refuse half, in the same file so the pair is read together: opening the
// sandbox opened the sandbox and nothing else. preview still needs a productId
// and a binding, and a check must not become a back door to one.
func TestSandboxCheckDidNotOpenTheSellerPath(t *testing.T) {
	for _, method := range []string{
		"kenwea.marketplace.preview",
		"kenwea.marketplace.purchase",
		"kenwea.orders.deliver",
	} {
		if err := rejectUnboundMutatingAgent(touristActor(), method); err == nil {
			t.Errorf("%s must still require an operator binding", method)
		}
	}
}

// A check burns real compute on our hardware for a caller who paid nothing, so
// it must not ride a cached session whose key was revoked since. That is what
// membership in mutatingTools buys -- fresh Authorization on every call -- and
// it is why the classification is asserted rather than assumed.
//
// The complement matters as much: it must NOT be idempotent-keyed. An
// Idempotency-Key would let a caller replay one key forever and receive the
// first verdict back, which for an artifact served from a URL whose contents can
// change is not a cached answer but a stale claim about bytes that are no longer
// there.
func TestSandboxCheckForcesFreshAuthButIsNotIdempotencyKeyed(t *testing.T) {
	if _, ok := mutatingTools["kenwea.sandbox.check"]; !ok {
		t.Error("kenwea.sandbox.check must be classified mutating so a revoked key cannot replay it from a cached session")
	}
	if _, ok := idempotentTools["kenwea.sandbox.check"]; ok {
		t.Error("kenwea.sandbox.check must not be idempotency-keyed: replaying a key would return a verdict about bytes the URL may no longer serve")
	}
}
