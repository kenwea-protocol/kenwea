package mcp

import "testing"

// TestTermsHistoryRecordsTheLiveFingerprint is the forcing function for the whole
// mechanism. Any change to the tool surface, the tourist tier, the commission, or
// a never-does guarantee moves the fingerprint, and this test fails until the
// transition is written down. Without it the log would silently fall behind the
// binary and become the stale document it exists to prevent -- which is how the
// spec header ended up claiming the wrong migration count.
func TestTermsHistoryRecordsTheLiveFingerprint(t *testing.T) {
	entries := termsHistory()
	if len(entries) == 0 {
		t.Fatal("terms-history.json parsed to no entries -- the embedded log is missing or malformed")
	}

	live := currentPublishedTerms().fingerprint()
	latest := entries[len(entries)-1]
	if latest.Fingerprint != live {
		t.Fatalf(`the published terms changed and were not recorded.

  live fingerprint    : %s
  last logged         : %s

Append an entry to apps/mcp-server/internal/mcp/terms-history.json with the live
fingerprint and the full terms it was computed from. A consumer holding the old
fingerprint can only be told WHAT moved if both sides are in the log.`, live, latest.Fingerprint)
	}
}

// Each entry must be internally consistent: the fingerprint it claims has to be
// the one its own stored inputs produce. Otherwise an entry could be appended by
// hand with a plausible-looking hash and every diff computed from it would be
// quietly wrong.
func TestTermsHistoryEntriesAreSelfConsistent(t *testing.T) {
	for i, e := range termsHistory() {
		if got := e.recomputeFingerprint(); got != e.Fingerprint {
			t.Errorf("entry %d (%s) claims fingerprint %s but its stored terms produce %s", i, e.RecordedAt, e.Fingerprint, got)
		}
	}
}

func TestTermsHistoryFingerprintsAreUniqueAndOrdered(t *testing.T) {
	seen := map[string]int{}
	for i, e := range termsHistory() {
		if prev, dup := seen[e.Fingerprint]; dup {
			t.Errorf("entry %d repeats the fingerprint from entry %d (%s) -- a transition to identical terms is not a transition", i, prev, e.Fingerprint)
		}
		seen[e.Fingerprint] = i
		if e.RecordedAt == "" {
			t.Errorf("entry %d (%s) has no recordedAt", i, e.Fingerprint)
		}
	}
}

// changedBetween is what a consumer actually reads, so it is tested against
// constructed terms rather than only against the single real entry.
func TestChangedBetweenDescribesEveryDimension(t *testing.T) {
	var before, after termsHistoryEntry
	before.Terms.ToolNames = []string{"a.one", "a.two"}
	before.Terms.TouristTools = []string{"a.one"}
	before.Terms.CommissionBps = 1200
	before.Terms.NeverDoes = []string{"never sells your data"}

	after.Terms.ToolNames = []string{"a.one", "a.three"}
	after.Terms.TouristTools = []string{}
	after.Terms.CommissionBps = 1500
	after.Terms.NeverDoes = []string{"never logs parameters"}

	changes := changedBetween(before, after)
	joined := ""
	for _, c := range changes {
		joined += c + "\n"
	}

	for _, want := range []string{
		"Tools added: a.three",
		"Tools removed: a.two",
		"No longer reachable without an operator: a.one",
		"Seller commission moved from 1200 to 1500",
		"New guarantee: never logs parameters",
		"WITHDRAWN guarantee: never sells your data",
	} {
		if !contains(joined, want) {
			t.Errorf("changedBetween did not report %q\ngot:\n%s", want, joined)
		}
	}
}

// A consumer on the current terms must be told nothing moved, not handed an
// empty list that reads the same as "we do not know".
func TestChangesSinceSeparatesCurrentFromUnknown(t *testing.T) {
	entries := termsHistory()
	if len(entries) == 0 {
		t.Skip("no history to exercise")
	}
	latest := entries[len(entries)-1].Fingerprint

	if changes, known := changesSince(latest); !known || len(changes) != 0 {
		t.Errorf("the current fingerprint must be known with no changes, got known=%v changes=%v", known, changes)
	}
	if _, known := changesSince("0000000000000000"); known {
		t.Error("a fingerprint that predates the log must report unknown rather than claiming nothing changed")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
