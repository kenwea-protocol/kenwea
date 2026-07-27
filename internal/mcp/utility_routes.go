package mcp

import (
	"fmt"
	"net/http"
)

func (s *Server) handleUtilityRoute(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/mcp/v1":
		return false
	case "/mcp/v1/health":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return true
	case "/metrics":
		writeMCPMetrics(w)
		return true
	// Keyless on purpose: an agent must be able to evaluate this before it
	// commits to anything, and it is what the agent hands to its operator.
	case descriptorPath:
		writeCapabilityDescriptor(w)
		return true
	// Redemption end of the proposal funnel. Keyless like the descriptor itself:
	// requiring a credential to say "a human acted on this" would defeat the
	// point, since the human acting has no credential yet.
	case proposalRedeemPath:
		redeemProposalToken(w, r)
		return true
	default:
		writeHTTPError(w, http.StatusNotFound, nil, "not_found", "route not found")
		return true
	}
}

func writeMCPMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_up Public MCP process availability.\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_up gauge\nkenwea_mcp_up 1\n")

	tools, tiers, registrations := toolUsage.snapshot()

	// Answers "did the keyless tourist pass actually get used, and for what?" --
	// previously unanswerable because reads were recorded nowhere.
	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_tool_calls_total MCP tool calls served since process start, by tool.\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_tool_calls_total counter\n")
	for _, entry := range tools {
		_, _ = fmt.Fprintf(w, "kenwea_mcp_tool_calls_total{tool=%q} %d\n", entry.Label, entry.Count)
	}

	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_tool_calls_by_tier_total MCP tool calls by caller tier (tourist = registered but unclaimed by an operator).\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_tool_calls_by_tier_total counter\n")
	for _, entry := range tiers {
		_, _ = fmt.Fprintf(w, "kenwea_mcp_tool_calls_by_tier_total{tier=%q} %d\n", entry.Label, entry.Count)
	}

	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_self_registrations_total Anonymous registerSelf calls served since process start.\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_self_registrations_total counter\n")
	_, _ = fmt.Fprintf(w, "kenwea_mcp_self_registrations_total %d\n", registrations)

	// The two observable ends of the agent -> operator proposal funnel. What
	// happens between a mint and a redemption is deliberately unmeasured.
	minted, redeemed, rejected, ageBuckets, termsClasses := proposalStats.snapshot()

	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_proposal_tokens_minted_total Descriptor fetches that minted an attribution token.\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_proposal_tokens_minted_total counter\n")
	_, _ = fmt.Fprintf(w, "kenwea_mcp_proposal_tokens_minted_total %d\n", minted)

	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_proposal_tokens_redeemed_total Proposals a human acted on (token redeemed).\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_proposal_tokens_redeemed_total counter\n")
	_, _ = fmt.Fprintf(w, "kenwea_mcp_proposal_tokens_redeemed_total %d\n", redeemed)

	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_proposal_tokens_rejected_total Redemption attempts refused (expired, malformed, or unsigned).\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_proposal_tokens_rejected_total counter\n")
	_, _ = fmt.Fprintf(w, "kenwea_mcp_proposal_tokens_rejected_total %d\n", rejected)

	// Shape matters more than the raw count: same-hour redemptions look like an
	// agent verifying its own recommendation, multi-day ones like a human
	// deliberating. Buckets are listed explicitly so the series exist at zero.
	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_proposal_redemption_age_total Time from descriptor fetch to redemption, bucketed.\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_proposal_redemption_age_total counter\n")
	for _, bucket := range []string{"under_1h", "under_1d", "under_3d", "over_3d"} {
		_, _ = fmt.Fprintf(w, "kenwea_mcp_proposal_redemption_age_total{bucket=%q} %d\n", bucket, ageBuckets[bucket])
	}

	// Did the human decide on the terms we still hold? A rising "stale" share
	// means the published terms are moving faster than people can act on them,
	// and identifies exactly who to notify on the next change.
	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_proposal_redemption_terms_total Redemptions by whether the minted-under terms still match the current ones.\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_proposal_redemption_terms_total counter\n")
	for _, class := range []string{termsClassCurrent, termsClassStale, termsClassUnknown} {
		_, _ = fmt.Fprintf(w, "kenwea_mcp_proposal_redemption_terms_total{class=%q} %d\n", class, termsClasses[class])
	}
}
