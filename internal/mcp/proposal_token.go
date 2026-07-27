package mcp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proposal attribution tokens measure the one funnel step that is otherwise
// unobservable: an agent recommending this server to its human operator.
//
// The design is johnnybucks's (Moltbook, 2026-07-27) and its key insight is what
// it does NOT try to do. Generation happens on a surface we will never see -- an
// agent writing a recommendation inside its own context -- so instrumenting it
// directly is impossible, and anyone claiming otherwise is guessing. Instead we
// observe the two ENDS: the descriptor fetch that starts a proposal, and the
// redemption when a human later acts on it. Proposal-generated is then BOUNDED
// between two real events rather than invented. The middle stays dark on purpose:
// the human deciding in private is the feature, not a measurement gap.
//
// PRIVACY, and why this does not contradict the published neverDoes guarantee
// that we never record which agent read what:
//   - the token carries only a mint timestamp and a random nonce;
//   - it carries no agent id, no key, no IP, and nothing derived from them;
//   - it is minted for every descriptor fetch, so it identifies a FETCH, not a
//     fetcher, and two fetches by the same agent are indistinguishable;
//   - redemption is counted in aggregate, exactly like tool usage.
//
// It attributes a funnel step. It cannot attribute a visitor.
//
// Stateless by construction: the token is self-describing and HMAC-signed, so
// verification needs no storage and no database round trip on either end. The
// accepted cost is that replayed redemptions are counted more than once; for a
// funnel metric that is a tolerable imprecision, and it is stated here rather
// than hidden.
const (
	proposalTokenPrefix = "kwp1"
	// Long enough for a human to actually read a proposal, discuss it, and edit a
	// config -- a ten-minute token would measure nothing but robots. Short enough
	// that it cannot quietly become a durable installation identifier.
	proposalTokenTTL = 7 * 24 * time.Hour
)

type proposalCounters struct {
	mu         sync.Mutex
	minted     uint64
	redeemed   uint64
	rejected   uint64
	ageBuckets map[string]uint64
	// Redemptions split by whether the human acted on the terms we still hold.
	// A rising "stale" share means the terms are moving faster than people decide.
	termsClasses map[string]uint64
}

var proposalStats = &proposalCounters{ageBuckets: map[string]uint64{}, termsClasses: map[string]uint64{}}

func (c *proposalCounters) recordMint() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minted++
}

// recordRedemption also buckets how long the proposal took to be acted on, which
// is the interesting shape: same-hour redemptions look like an agent verifying
// its own recommendation, multi-day ones look like a human deliberating.
func (c *proposalCounters) recordRedemption(age time.Duration, termsClass string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.redeemed++
	c.ageBuckets[ageBucket(age)]++
	c.termsClasses[termsClass]++
}

func (c *proposalCounters) recordRejection() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejected++
}

func (c *proposalCounters) snapshot() (minted, redeemed, rejected uint64, buckets, classes map[string]uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	buckets = make(map[string]uint64, len(c.ageBuckets))
	for k, v := range c.ageBuckets {
		buckets[k] = v
	}
	classes = make(map[string]uint64, len(c.termsClasses))
	for k, v := range c.termsClasses {
		classes[k] = v
	}
	return c.minted, c.redeemed, c.rejected, buckets, classes
}

func ageBucket(age time.Duration) string {
	switch {
	case age < time.Hour:
		return "under_1h"
	case age < 24*time.Hour:
		return "under_1d"
	case age < 3*24*time.Hour:
		return "under_3d"
	default:
		return "over_3d"
	}
}

func proposalSecret() []byte {
	return []byte(strings.TrimSpace(os.Getenv("KENWEA_PROPOSAL_TOKEN_SECRET")))
}

// mintProposalToken returns a signed token, or "" when no secret is configured.
// Degrading to no token is deliberate: a weak fallback secret would make the
// signature decorative, and an unsigned token would let anyone inflate the
// redemption count. No measurement is better than a forgeable one.
// The minted-under terms fingerprint is bound INTO the token, not just the mint
// time. Wall-clock age answers "how long did they think about it"; the
// fingerprint answers the question that actually matters -- did the human decide
// on the terms we still hold? A token minted under fingerprint X and redeemed
// after we moved to Y is a stale-terms install: its own category, and precisely
// the population worth notifying on the next change. No expiry window can find
// that group, because staleness here is semantic, not temporal.
func mintProposalToken(now time.Time, fingerprint string) string {
	secret := proposalSecret()
	if len(secret) == 0 {
		return ""
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	payload := fmt.Sprintf("%d.%s.%s", now.Unix(), fingerprint, base64.RawURLEncoding.EncodeToString(nonce))
	return proposalTokenPrefix + "." + payload + "." + signProposalPayload(secret, payload)
}

func signProposalPayload(secret []byte, payload string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type proposalRedemption struct {
	Valid bool
	Age   time.Duration
	// MintedUnderFingerprint is the terms fingerprint that was live when this
	// token was issued. Empty for legacy tokens minted before terms binding.
	MintedUnderFingerprint string
	// TermsClass is the funnel category this redemption belongs to:
	// "current" (decided on the terms we still hold), "stale" (terms moved
	// between reading and acting), or "unknown" (legacy token, no binding).
	TermsClass string
	Reason     string
}

const (
	termsClassCurrent = "current"
	termsClassStale   = "stale"
	termsClassUnknown = "unknown"
)

// verifyProposalToken checks signature, age, and which terms the token was
// minted under -- all without stored state.
//
// Legacy 4-part tokens (minted before terms binding) are still honoured rather
// than rejected: the descriptor promised those a 7-day life, and breaking that
// promise to tidy up a format would be the small dishonesty this whole mechanism
// exists to avoid. They redeem as "unknown" terms class.
func verifyProposalToken(token string, now time.Time, currentFingerprint string) proposalRedemption {
	secret := proposalSecret()
	if len(secret) == 0 {
		return proposalRedemption{Reason: "attribution_disabled"}
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if parts[0] != proposalTokenPrefix {
		return proposalRedemption{Reason: "malformed"}
	}

	var payload, signature, mintedFingerprint string
	switch len(parts) {
	case 5: // prefix . unix . fingerprint . nonce . signature
		payload = parts[1] + "." + parts[2] + "." + parts[3]
		signature = parts[4]
		mintedFingerprint = parts[2]
	case 4: // legacy: prefix . unix . nonce . signature
		payload = parts[1] + "." + parts[2]
		signature = parts[3]
	default:
		return proposalRedemption{Reason: "malformed"}
	}

	expected := signProposalPayload(secret, payload)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) != 1 {
		return proposalRedemption{Reason: "bad_signature"}
	}
	mintedUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return proposalRedemption{Reason: "malformed"}
	}
	age := now.Sub(time.Unix(mintedUnix, 0))
	if age < 0 {
		return proposalRedemption{Reason: "minted_in_future"}
	}
	if age > proposalTokenTTL {
		return proposalRedemption{Reason: "expired", Age: age}
	}

	class := termsClassUnknown
	switch {
	case mintedFingerprint == "":
		class = termsClassUnknown
	case mintedFingerprint == currentFingerprint:
		class = termsClassCurrent
	default:
		class = termsClassStale
	}
	return proposalRedemption{
		Valid:                  true,
		Age:                    age,
		MintedUnderFingerprint: mintedFingerprint,
		TermsClass:             class,
	}
}

// verifyCommands returns the "check me, don't trust me" list, with the
// attribution token folded into the redemption call when one was minted.
func verifyCommands(token string) []string {
	commands := []string{
		"npx -y @kenwea/mcp doctor",
		"curl -s https://mcp.kenwea.com/mcp/v1/health",
		descriptorPath + " (this document)",
	}
	if token != "" {
		commands = append(commands,
			"curl -s https://mcp.kenwea.com"+proposalRedeemPath+"?token="+token+"   # optional: tells us this proposal was acted on")
	}
	return commands
}

// attributionBlock documents the token in-band so nobody has to guess what it is
// or whether it is safe to remove.
func attributionBlock(token string) map[string]any {
	if token == "" {
		return map[string]any{
			"enabled": false,
			"note":    "Proposal attribution is not configured on this deployment.",
		}
	}
	return map[string]any{
		"enabled":      true,
		"token":        token,
		"redeemUrl":    "https://mcp.kenwea.com" + proposalRedeemPath + "?token=" + token,
		"expiresInSec": int(proposalTokenTTL.Seconds()),
		"identifies":   "this descriptor fetch, and the terms fingerprint it was minted under",
		"termsBinding": "The token carries the termsFingerprint that was live when you fetched this document. Redeeming it tells us whether you decided on the terms still in force or on ones that have since moved -- and the response tells YOU the same thing, so you can re-read before installing against a changed document.",
		"doesNotIdentify": []string{
			"the agent that fetched it",
			"an agent key, IP address, or anything derived from them",
			"which tools were read",
		},
		"optional": "Stripping this block changes nothing about how the server behaves.",
		"why":      "Proposal generation happens inside an agent's own context, where it cannot be observed. Redeeming this token marks the far end of that step, so the funnel is bounded by two real events instead of estimated.",
	}
}

const proposalRedeemPath = "/.well-known/mcp-proposal-redeem"

// redeemProposalToken marks the far end of the proposal funnel. It reports
// honestly why a token was refused rather than silently returning ok, so a human
// following the verify instructions can tell "expired" from "wrong link".
func redeemProposalToken(w http.ResponseWriter, r *http.Request) {
	current := currentPublishedTerms().fingerprint()
	result := verifyProposalToken(r.URL.Query().Get("token"), time.Now(), current)
	if !result.Valid {
		proposalStats.recordRejection()
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"redeemed": false,
			"reason":   result.Reason,
			"note":     "Nothing is broken on your side -- this only affects whether we can count that the proposal was acted on.",
		})
		return
	}
	proposalStats.recordRedemption(result.Age, result.TermsClass)

	body := map[string]any{
		"redeemed":   true,
		"termsClass": result.TermsClass,
		"note":       "Recorded that a descriptor-based proposal was acted on. No identity was stored: this token described a fetch, not a fetcher.",
	}
	// Telling the redeemer their terms moved is useful to THEM, not just to us --
	// they are about to install against a document that has since changed.
	switch result.TermsClass {
	case termsClassStale:
		body["mintedUnderFingerprint"] = result.MintedUnderFingerprint
		body["currentFingerprint"] = current
		// The whole point of the transition log: answer WHAT moved, not just that
		// something did. "Go re-read everything" leaves the only question worth
		// asking -- did the part I depend on change? -- unanswered.
		if changes, known := changesSince(result.MintedUnderFingerprint); known {
			body["termsChanged"] = changes
		} else {
			body["termsChanged"] = "The published terms changed, but the fingerprint this token was minted under predates the transition log, so what moved cannot be reconstructed. Re-read " + descriptorPath + " in full."
		}
	case termsClassCurrent:
		body["currentFingerprint"] = current
		body["termsChanged"] = "None. The terms you read are the terms in force."
	case termsClassUnknown:
		body["currentFingerprint"] = current
		body["termsChanged"] = "This token predates terms binding, so we cannot tell whether the terms moved. Re-read the descriptor to be sure."
	}
	writeJSON(w, http.StatusOK, body)
}
