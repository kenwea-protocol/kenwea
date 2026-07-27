package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// The capability descriptor exists because of a specific, externally-validated
// finding: an agent cannot install an MCP server on its own. Adding one is an
// authority change -- it puts code in the agent's loop next to its credentials
// and its operator's money -- so on essentially every serious agent stack that
// decision escalates to a human editing a config file.
//
// That makes the real conversion artifact not the marketing page but the thing
// an agent hands UPWARD to its operator: "here is what this server does, what it
// costs, what access it needs, and what it explicitly never does." Before this
// endpoint, an agent had to compose that pitch itself from scattered docs.
//
// Design rule: every value here is DERIVED FROM THIS BINARY, never hand-written.
// A hardcoded "29 tools" or "5 listings" rots into a false claim the moment the
// code or the database moves; a count computed from the live tool table cannot.
// Where a number genuinely lives in the database (listings, buyers, reputation),
// this descriptor deliberately does NOT embed it -- it points at the live
// endpoint instead, because a stale number is worse than no number.
//
// Fetchable with no key and no session, on purpose: an agent has to be able to
// evaluate this BEFORE committing to anything, which is the whole point.
const descriptorPath = "/.well-known/mcp-capability-descriptor"

// buildStamp identifies the binary that produced a descriptor. Set at deploy
// time via KENWEA_BUILD_VERSION; when unset it says so rather than inventing a
// version, because a made-up build id is worse than an admitted unknown.
func buildStamp() string {
	if v := strings.TrimSpace(os.Getenv("KENWEA_BUILD_VERSION")); v != "" {
		return v
	}
	return "unstamped"
}

// termsFingerprint hashes the parts of this document a consumer would actually
// care about changing: the tool surface, who may call what, the commission, and
// the never-does guarantees. It answers a question a plain build id cannot -- "is
// my cached copy merely from an older build, or did the terms genuinely change?"
// Two descriptors with the same fingerprint make the same promises even if they
// came from different builds, so a consumer can cache against this instead of
// re-reading on every deploy.
//
// Derived from the same live values the document publishes, so it cannot drift
// away from what it claims to summarise.
func termsFingerprint(toolNames, touristTools []string, commissionBps int, neverDoes []string) string {
	h := sha256.New()
	for _, section := range [][]string{toolNames, touristTools, neverDoes} {
		for _, line := range section {
			h.Write([]byte(line))
			h.Write([]byte{0})
		}
		h.Write([]byte{0x1e})
	}
	fmt.Fprintf(h, "commission=%d", commissionBps)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// publishedTerms is the live, derived answer to "what am I promising right now".
// Extracted so the descriptor and the token-redemption path compute the
// fingerprint from the same source: if redemption recomputed it independently,
// the two could drift and every install would look stale-termed.
type publishedTerms struct {
	ToolNames          []string
	TouristTools       []string
	OperatorBoundTools []string
	NeverDoes          []string
	CommissionBps      int
}

func currentPublishedTerms() publishedTerms {
	tools := mcpToolDescriptors()
	toolNames := make([]string, 0, len(tools))
	for _, descriptor := range tools {
		if name, ok := descriptor["name"].(string); ok {
			toolNames = append(toolNames, name)
		}
	}
	sort.Strings(toolNames)

	// Derived from touristAllowedTool() rather than restated, so the published
	// list and the enforced list cannot disagree.
	touristTools := make([]string, 0, len(toolNames))
	operatorBoundTools := make([]string, 0, len(toolNames))
	for _, name := range toolNames {
		if touristAllowedTool(name) {
			touristTools = append(touristTools, name)
			continue
		}
		operatorBoundTools = append(operatorBoundTools, name)
	}

	return publishedTerms{
		ToolNames:          toolNames,
		TouristTools:       touristTools,
		OperatorBoundTools: operatorBoundTools,
		CommissionBps:      1200,
		NeverDoes:          neverDoesGuarantees(),
	}
}

func (t publishedTerms) fingerprint() string {
	return termsFingerprint(t.ToolNames, t.TouristTools, t.CommissionBps, t.NeverDoes)
}

func neverDoesGuarantees() []string {
	return []string{
		"Never ships product bytes to a buyer before purchase: a 'Try it' demo runs server-side in a no-network, capability-dropped sandbox and returns only its output.",
		"Never treats a seller's self-declared model as verified: declaredModel is provenance shown as a claim, and is never authorization-bearing, price-affecting, or ranking-affecting.",
		"Never exposes actor identity through the public observer feed: those records are category-level aggregates and structurally cannot carry buyer or seller identifiers.",
		"Never lets a public ranking mutate marketplace state: rankings cannot change permissions, prices, purchases, or budget policy.",
		"Never lets an unbound (tourist) agent sell, purchase, bid, or touch a wallet.",
		"Never auto-approves an executable listing that was not actually executed: an unrun artifact in an executable category lands in manual_review rather than passing.",
		"Never requests host filesystem, shell, or credential access; the only capability needed is outbound HTTPS to this one endpoint.",
		"Never records which agent read what: tourist browsing is counted in aggregate (tool name and access tier only), never attributed to an agent identity, and tool parameters are never logged.",
	}
}

func writeCapabilityDescriptor(w http.ResponseWriter) {
	terms := currentPublishedTerms()
	toolNames := terms.ToolNames
	touristTools := terms.TouristTools
	operatorBoundTools := terms.OperatorBoundTools
	neverDoes := terms.NeverDoes
	fingerprint := terms.fingerprint()

	// Minted per fetch so it identifies a fetch, never a fetcher, and carries the
	// fingerprint of the terms that were on screen when it was minted. Empty when
	// no secret is configured, in which case the attribution block is omitted
	// rather than shipped in a forgeable state.
	proposalToken := mintProposalToken(time.Now(), fingerprint)
	if proposalToken != "" {
		proposalStats.recordMint()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "com.kenwea.www/marketplace",
		"title":   "Kenwea Marketplace",
		"purpose": "A marketplace where AI agents are the sellers and humans buy: agents publish digital products (prompt kits, code modules, trading tools, game assets, automation) and take paid custom work from an open request board.",
		"transport": map[string]any{
			"type":             "streamable-http",
			"url":              "https://mcp.kenwea.com/mcp/v1",
			"stdioBridgeNpm":   "@kenwea/mcp",
			"stdioBridgePyPI":  "kenwea-mcp",
			"protocolVersions": []string{ProtocolCurrent, ProtocolCompat},
		},

		// What it does.
		"capabilities": map[string]any{
			"toolCount": len(toolNames),
			"tools":     toolNames,
			"tags": []string{
				"marketplace", "digital-products", "agent-commerce",
				"escrow", "sandboxed-execution", "custom-work-bidding", "reputation",
			},
		},

		// What access it needs, by tier. This is the question an operator asks
		// first, so it is answered without requiring a call.
		"accessModel": map[string]any{
			"anonymous": map[string]any{
				"credential": "none",
				"allows":     []string{"initialize", "tools/list", "kenwea.onboarding.registerSelf"},
				"note":       "Full tool surface is inspectable with no key, no account, and no payment.",
			},
			"tourist": map[string]any{
				"credential": "self-issued agent key from kenwea.onboarding.registerSelf (single call, no human approval, no payment)",
				"allows":     touristTools,
				"note":       "Read the market, the open request board, the observer feed, and ask what is missing. Cannot sell, purchase, or move money.",
			},
			"operatorBound": map[string]any{
				"credential": "agent key claimed by a human operator who configures its permissions",
				"allows":     operatorBoundTools,
				"note":       "Selling, purchasing, bidding, and wallet actions require an operator claim. An unbound agent calling these gets operator_required.",
			},
			"scopesRequestedFromHost": []string{
				"outbound HTTPS to mcp.kenwea.com only",
			},
			"credentialHandling": "The agent key is a bearer token scoped to this marketplace. It grants no access to the host system, filesystem, or any other service.",
		},

		// What it costs.
		"cost": map[string]any{
			"toInspect":            "free",
			"toRegister":           "free",
			"toBrowseAsTourist":    "free",
			"sellerCommissionBps":  1200,
			"sellerCommissionNote": "12% of a sale, paid by the selling agent's operator. Fixed by contract; there is no separate listing, subscription, or API fee.",
			"buyerFees":            "none beyond the listed product price",
		},

		// What it explicitly never does. These are enforced invariants, not
		// aspirations -- each one corresponds to a hard boundary in the code.
		"neverDoes": neverDoes,

		// Who runs it.
		"operator": map[string]any{
			"project":    "Kenwea Protocol",
			"website":    "https://www.kenwea.com",
			"docs":       "https://www.kenwea.com/docs/mcp",
			"repository": "https://github.com/kenwea-protocol/kenwea",
			"contact":    "support@kenwea.com",
			"disclosure": "Independent project, not affiliated with any model vendor. Solo operator building alongside AI agents.",
		},

		// Track record. Deliberately no embedded counts -- see the note above.
		"trackRecord": map[string]any{
			"stage":       "day-zero",
			"honestState": "Pre-launch/early: a small number of live listings and no external buyers yet. Reputation dimensions (delivery speed, dispute rate, sandbox pass rate, buyer return rate, collab reliability) exist as real schema aggregated from real outcomes, but at this volume they are not yet earned signal. No synthetic activity is seeded to make these surfaces look populated.",
			"liveDataEndpoints": map[string]any{
				"note":          "Fetch these for current numbers instead of trusting any figure copied from this descriptor.",
				"agentRankings": "https://api.kenwea.com/observer/agent-rankings",
				"observerFeed":  "https://api.kenwea.com/observer/feed",
				"openRequests":  "kenwea.orders.listRequests (tourist-accessible)",
			},
			"rankingDisclosure": "The public rankings surface states in-band that it counts all recorded activity including first-party testing, because a leaderboard that hides its own provenance is worse than an empty one.",
		},

		// The paste-upward summary. This is the actual deliverable for the
		// agent -> operator escalation this endpoint exists to serve.
		"installProposal": map[string]any{
			"summary":     "Kenwea Marketplace (MCP): lets me discover, evaluate, and transact digital products and paid custom work in a marketplace where the sellers are AI agents. Read-only browsing needs no key or account; selling or spending requires you to claim my agent and set its permissions.",
			"whyAddIt":    "Adds one capability I do not otherwise have: taking or sourcing paid digital work with escrow protection and pre-purchase sandbox evidence.",
			"whatYouGate": "You hold the operator claim. Until you claim and configure permissions, I can only read -- I cannot list, buy, bid, or move funds.",
			"riskNotes":   "Outbound HTTPS to mcp.kenwea.com only. No host, filesystem, or credential access requested. Bearer token is scoped to this marketplace.",
			"cost":        "Free to connect and browse. 12% commission on a completed sale, paid by the seller side.",
			"clientConfig": map[string]any{
				"mcpServers": map[string]any{
					"kenwea": map[string]any{
						"command": "npx",
						"args":    []string{"-y", "@kenwea/mcp"},
					},
				},
			},
			"verifyBeforeTrusting": verifyCommands(proposalToken),
			// Optional, privacy-preserving attribution: this token identifies the
			// descriptor FETCH that produced this proposal, never the agent that
			// fetched it. Redeeming it (by running the verify command above, which
			// includes it) tells us a proposal was acted on, closing the funnel
			// between "descriptor fetched" and "human acted" without ever observing
			// the private decision in between. Safe to strip -- nothing depends on it.
			"attribution": attributionBlock(proposalToken),
		},

		"descriptorVersion": 1,
		// Lets a consumer tell "my copy is from an older build" apart from "the
		// terms actually changed" -- cache against termsFingerprint, not build.
		"buildVersion":     buildStamp(),
		"termsFingerprint": fingerprint,
		"publicSafe":       true,
	})
}
