package mcp

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The terms transition log closes the gap johnnybucks named on Moltbook
// (2026-07-27) right after the fingerprint itself shipped: a consumer whose
// cached copy carries an old fingerprint learns THAT the terms moved and never
// WHAT moved. "Something changed, go re-read everything" is barely better than no
// signal, because the one question it leaves unanswered -- did the thing I
// actually depend on change? -- is the only one worth asking.
//
// WHY THIS IS A COMMITTED FILE AND NOT RUNTIME STATE. This server has no
// database, no volume, and writes nothing to disk; every counter in it resets on
// restart. A transition log kept in memory would lose its history on the next
// deploy, which is precisely when terms are most likely to have changed. Terms
// change when the binary changes, so the history belongs where the binary's
// history already lives. Git survives a redeploy by construction, and the log is
// publicly auditable as a side effect rather than as an extra promise.
//
// WHY IT STORES INPUTS AND NOT A SUMMARY. Each entry carries the full tool list,
// tourist list, commission and never-does text the fingerprint was computed from.
// A hand-written "what changed" line would be a claim maintained by hand next to
// a value computed by machine, which is exactly the drift this codebase keeps
// finding. Storing the inputs makes the diff DERIVED: changedBetween computes it,
// so it cannot describe a change that did not happen or miss one that did.
//
//go:embed terms-history.json
var termsHistoryRaw []byte

type termsHistoryEntry struct {
	Fingerprint string `json:"fingerprint"`
	RecordedAt  string `json:"recordedAt"`
	Note        string `json:"note,omitempty"`
	Terms       struct {
		ToolNames     []string `json:"toolNames"`
		TouristTools  []string `json:"touristTools"`
		CommissionBps int      `json:"commissionBps"`
		NeverDoes     []string `json:"neverDoes"`
	} `json:"terms"`
}

type termsHistoryDoc struct {
	Entries []termsHistoryEntry `json:"entries"`
}

func termsHistory() []termsHistoryEntry {
	var doc termsHistoryDoc
	if err := json.Unmarshal(termsHistoryRaw, &doc); err != nil {
		// Embedded and covered by a test, so this is unreachable in a built
		// binary. Returning empty rather than panicking keeps a malformed log from
		// taking the whole descriptor down: a missing history is a degraded
		// answer, an unavailable descriptor is no answer at all.
		return nil
	}
	return doc.Entries
}

// recomputeFingerprint derives the fingerprint from an entry's own stored inputs.
// If this disagrees with the entry's recorded fingerprint, the entry is lying
// about itself -- checked by the test rather than trusted.
func (e termsHistoryEntry) recomputeFingerprint() string {
	return termsFingerprint(e.Terms.ToolNames, e.Terms.TouristTools, e.Terms.CommissionBps, e.Terms.NeverDoes)
}

func setDifference(before, after []string) (added, removed []string) {
	in := func(list []string) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, v := range list {
			m[v] = true
		}
		return m
	}
	b, a := in(before), in(after)
	for _, v := range after {
		if !b[v] {
			added = append(added, v)
		}
	}
	for _, v := range before {
		if !a[v] {
			removed = append(removed, v)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// changedBetween describes, in plain sentences, exactly what moved between two
// recorded sets of terms. Every line is derived from the stored inputs.
func changedBetween(before, after termsHistoryEntry) []string {
	var changes []string

	if added, removed := setDifference(before.Terms.ToolNames, after.Terms.ToolNames); len(added) > 0 || len(removed) > 0 {
		if len(added) > 0 {
			changes = append(changes, "Tools added: "+strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			changes = append(changes, "Tools removed: "+strings.Join(removed, ", "))
		}
	}

	// A tool moving between tiers is the change most likely to break a caller
	// that assumed it could reach something without an operator, so it is
	// reported separately from the tool list rather than folded into it.
	if added, removed := setDifference(before.Terms.TouristTools, after.Terms.TouristTools); len(added) > 0 || len(removed) > 0 {
		if len(added) > 0 {
			changes = append(changes, "Now reachable without an operator: "+strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			changes = append(changes, "No longer reachable without an operator: "+strings.Join(removed, ", "))
		}
	}

	if before.Terms.CommissionBps != after.Terms.CommissionBps {
		changes = append(changes, fmt.Sprintf("Seller commission moved from %d to %d basis points", before.Terms.CommissionBps, after.Terms.CommissionBps))
	}

	if added, removed := setDifference(before.Terms.NeverDoes, after.Terms.NeverDoes); len(added) > 0 || len(removed) > 0 {
		for _, guarantee := range added {
			changes = append(changes, "New guarantee: "+guarantee)
		}
		// A withdrawn guarantee is the most serious entry this log can carry, so
		// it is spelled out rather than counted.
		for _, guarantee := range removed {
			changes = append(changes, "WITHDRAWN guarantee: "+guarantee)
		}
	}

	return changes
}

// changesSince returns what moved between a caller's fingerprint and the current
// terms. The second return value is false when the fingerprint is not in the log
// at all -- which is an honest answer, not an error: it predates the log, and
// this file will not guess at history it never recorded.
func changesSince(fingerprint string) ([]string, bool) {
	entries := termsHistory()
	if len(entries) == 0 {
		return nil, false
	}
	from := -1
	for i, e := range entries {
		if e.Fingerprint == fingerprint {
			from = i
			break
		}
	}
	if from < 0 {
		return nil, false
	}

	latest := entries[len(entries)-1]
	if entries[from].Fingerprint == latest.Fingerprint {
		return nil, true
	}
	return changedBetween(entries[from], latest), true
}

// termsHistoryBlock is the public shape: the recorded fingerprints in order, and
// what moved at each step. Deliberately no per-entry prose beyond the seed note.
func termsHistoryBlock() []map[string]any {
	entries := termsHistory()
	out := make([]map[string]any, 0, len(entries))
	for i, e := range entries {
		item := map[string]any{
			"fingerprint": e.Fingerprint,
			"recordedAt":  e.RecordedAt,
		}
		if e.Note != "" {
			item["note"] = e.Note
		}
		if i > 0 {
			item["changedFromPrevious"] = changedBetween(entries[i-1], e)
		}
		out = append(out, item)
	}
	return out
}
