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
	// The path MCP directory crawlers actually ask for. The 2026-07-30 access log
	// showed AgenstryBot, aisec-registry, mcpgrade-probe, 402explorer and
	// agent-tools.cloud-crawler requesting /.well-known/mcp and getting 404 while
	// the same document sat one path over under a Kenwea-specific name. We were
	// being indexed and failing the request that would have listed us.
	// `.json` added 2026-08-06: 28 requests asked for the descriptor by that exact
	// filename and got 404 while the same document sat one character away. Adding an
	// alias a crawler actually asks for is not the same as inventing a document --
	// this is the one we already publish, under the other name for it.
	case "/.well-known/mcp", "/.well-known/mcp.json",
		"/mcp/v1/.well-known/mcp", "/mcp/v1/.well-known/mcp.json":
		writeCapabilityDescriptor(w)
		return true
	// Asked for by every crawler in that log and previously 404. Serving it is how
	// a crawler learns what it may index; the descriptor is explicitly inviting.
	case "/robots.txt":
		writeRobotsTxt(w)
		return true
	// The front door. Measured 2026-08-06: 611 requests to `/` and every one of them
	// answered `route not found` -- 485 of those from browsers, which is a person
	// who heard the hostname and pasted it in. A JSON-RPC error is the correct
	// answer to a bad RPC call and the wrong answer to someone arriving.
	//
	// Content-negotiated because both kinds of visitor are real: a machine gets the
	// same facts as JSON, and neither version says anything the descriptor does not.
	case "/":
		writeRootIndex(w, r)
		return true
	// A2A Agent Card discovery. Asked for 526 times in the week to 2026-09-25
	// (agent-card.json is the A2A 0.3 and 1.0 path, agent.json the older one).
	// We answer 404 because we have no Agent Card to give: a card must name an A2A
	// endpoint that implements the A2A operations (send message, get task), and
	// this server speaks MCP, not A2A. Publishing a card pointing at the MCP
	// endpoint would send every A2A client into calls that fail. The body turns
	// the 404 into a pointer, so the crawler or agent that came looking learns
	// what we do speak and where to read about it.
	case "/.well-known/agent-card.json", "/.well-known/agent.json",
		"/mcp/v1/.well-known/agent-card.json", "/mcp/v1/.well-known/agent.json":
		writeNotAnA2AAgent(w)
		return true
	// Glama's ownership file for a remote connector. Kenwea is listed on Glama as
	// connector com.kenwea.www/marketplace and has not been claimed. The claim
	// token comes from the claim panel after the operator signs in to Glama; it
	// is set as KENWEA_GLAMA_CLAIM and served here. Until it is set this stays a
	// 404, never an invented or placeholder token.
	case "/.well-known/glama.json":
		writeGlamaClaim(w)
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

// writeRobotsTxt tells crawlers what they may read. Everything this server exposes
// without a credential is meant to be read without one -- that is A3.1, the rule that
// the full tool surface is inspectable anonymously -- so the permissive answer is the
// honest one rather than a courtesy.
//
// The endpoint that takes JSON-RPC is disallowed because a crawler GETting it learns
// nothing and gets an error; pointing at the descriptor instead sends it where the
// answer actually is.
func writeRobotsTxt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(`User-agent: *
Allow: /.well-known/
Allow: /mcp/v1/health
Disallow: /mcp/v1
Disallow: /mcp/v1/

# What a crawler indexing this server probably wants:
#   /.well-known/mcp            capability descriptor (tools, tiers, cost, terms fingerprint)
#   /mcp/v1/health              liveness
#   /mcp/v1                     JSON-RPC over HTTP; POST only, not fetchable
#
# Deliberately absent, because absence is the truthful answer rather than an oversight:
#   /.well-known/oauth-*        this server is not OAuth-protected. Auth is a bearer
#                               agent key from kenwea.onboarding.registerSelf, and
#                               publishing OAuth metadata for an authorization server
#                               that does not exist would advertise a flow that cannot
#                               be completed.
#   agent.json, agent-card.json this server does not speak A2A. An agent card would
#                               claim a protocol it does not implement.
`))
}
