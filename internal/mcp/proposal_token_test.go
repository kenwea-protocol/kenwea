package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProposalTokenRoundTrips(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	now := time.Now()
	token := mintProposalToken(now, "fp-current")
	if token == "" {
		t.Fatal("expected a token when a secret is configured")
	}
	result := verifyProposalToken(token, now.Add(2*time.Hour), "fp-current")
	if !result.Valid {
		t.Fatalf("freshly minted token rejected: %s", result.Reason)
	}
	if result.Age < time.Hour || result.Age > 3*time.Hour {
		t.Errorf("age looks wrong: %v", result.Age)
	}
}

// Without a secret the token must be omitted, never emitted unsigned -- an
// unsigned token would let anyone inflate the redemption count, which is worse
// than having no measurement.
func TestProposalTokenOmittedWithoutSecret(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "")
	if got := mintProposalToken(time.Now(), "fp-current"); got != "" {
		t.Fatalf("expected no token without a secret, got %q", got)
	}
	if r := verifyProposalToken("kwp1.1.fp.abc.def", time.Now(), "fp-current"); r.Valid {
		t.Fatal("verification must not succeed while attribution is disabled")
	}
}

func TestProposalTokenRejectsForgeryAndExpiry(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	now := time.Now()
	token := mintProposalToken(now, "fp-current")

	cases := map[string]struct {
		token string
		when  time.Time
		want  string
	}{
		"tampered signature": {token[:len(token)-4] + "AAAA", now, "bad_signature"},
		"malformed":          {"not-a-token", now, "malformed"},
		"wrong prefix":       {strings.Replace(token, "kwp1", "kwp9", 1), now, "malformed"},
		"expired":            {token, now.Add(proposalTokenTTL + time.Hour), "expired"},
		"minted in future":   {mintProposalToken(now.Add(time.Hour), "fp-current"), now, "minted_in_future"},
	}
	for name, tc := range cases {
		got := verifyProposalToken(tc.token, tc.when, "fp-current")
		if got.Valid {
			t.Errorf("%s: should have been refused", name)
			continue
		}
		if got.Reason != tc.want {
			t.Errorf("%s: reason %q, want %q", name, got.Reason, tc.want)
		}
	}
}

// A token minted under one secret must not verify under another, so rotating the
// secret invalidates outstanding tokens rather than silently accepting them.
func TestProposalTokenIsSecretBound(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "secret-one")
	token := mintProposalToken(time.Now(), "fp-current")
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "secret-two")
	if r := verifyProposalToken(token, time.Now(), "fp-current"); r.Valid {
		t.Fatal("token verified under a different secret")
	}
}

// The privacy claim is the load-bearing part: the token must contain nothing
// derived from the caller.
func TestProposalTokenCarriesNoIdentity(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	token := mintProposalToken(time.Now(), "fp-current")
	for _, forbidden := range []string{"agent_", "kw_agent", "op_", "@"} {
		if strings.Contains(token, forbidden) {
			t.Errorf("token leaks %q: %s", forbidden, token)
		}
	}
	// Two fetches must be indistinguishable from each other -- if the token were
	// derived from the caller, repeated mints would collide.
	if mintProposalToken(time.Now(), "fp-current") == token {
		t.Error("tokens must be unique per fetch, not per caller")
	}
}

func TestRedeemEndpointCountsAndRefusesHonestly(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	original := proposalStats
	proposalStats = &proposalCounters{ageBuckets: map[string]uint64{}, termsClasses: map[string]uint64{}}
	defer func() { proposalStats = original }()

	server := NewServer(StaticAuthenticator{})
	token := mintProposalToken(time.Now(), "fp-current")

	// Valid redemption.
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, proposalRedeemPath+"?token="+token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid redemption returned %d: %s", rec.Code, rec.Body.String())
	}
	var ok map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ok)
	if ok["redeemed"] != true {
		t.Errorf("expected redeemed=true, got %v", ok)
	}

	// Invalid redemption must say WHY, not silently return ok.
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, proposalRedeemPath+"?token=garbage", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid redemption returned %d", rec.Code)
	}
	var bad map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &bad)
	if bad["redeemed"] != false || bad["reason"] == nil {
		t.Errorf("refusal must report a reason, got %v", bad)
	}

	minted, redeemed, rejected, buckets, _ := proposalStats.snapshot()
	if redeemed != 1 || rejected != 1 {
		t.Errorf("counters wrong: redeemed=%d rejected=%d", redeemed, rejected)
	}
	if buckets["under_1h"] != 1 {
		t.Errorf("age bucket not recorded: %v", buckets)
	}
	_ = minted
}

// The descriptor must carry the token and document it as optional, so nobody has
// to guess whether stripping it breaks anything.
func TestDescriptorCarriesOptionalAttributionBlock(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	body := fetchDescriptor(t)
	proposal, _ := body["installProposal"].(map[string]any)
	attribution, ok := proposal["attribution"].(map[string]any)
	if !ok {
		t.Fatal("installProposal must carry an attribution block")
	}
	if attribution["enabled"] != true {
		t.Fatalf("attribution should be enabled with a secret set: %v", attribution)
	}
	if attribution["token"] == nil || attribution["token"] == "" {
		t.Error("attribution block must include the token")
	}
	if attribution["optional"] == nil {
		t.Error("attribution must state that it is safe to strip")
	}
	// The privacy boundary has to be published, not just implemented.
	raw, _ := json.Marshal(attribution["doesNotIdentify"])
	if !strings.Contains(string(raw), "agent that fetched") {
		t.Errorf("attribution must state it cannot identify the fetcher: %s", raw)
	}
}

func TestDescriptorAttributionDisabledWithoutSecret(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "")
	body := fetchDescriptor(t)
	proposal, _ := body["installProposal"].(map[string]any)
	attribution, _ := proposal["attribution"].(map[string]any)
	if attribution["enabled"] != false {
		t.Errorf("attribution must report itself disabled when unconfigured: %v", attribution)
	}
	if attribution["token"] != nil {
		t.Error("no token may be published when attribution is disabled")
	}
}

// The counters were wired but silently missing from /metrics on the first
// deploy -- a live check caught it. This pins the exposure so it cannot regress.
func TestMetricsExposesProposalFunnelCounters(t *testing.T) {
	original := proposalStats
	proposalStats = &proposalCounters{ageBuckets: map[string]uint64{}, termsClasses: map[string]uint64{}}
	defer func() { proposalStats = original }()

	proposalStats.recordMint()
	proposalStats.recordMint()
	proposalStats.recordRedemption(30*time.Minute, termsClassCurrent)
	proposalStats.recordRejection()

	rec := httptest.NewRecorder()
	writeMCPMetrics(rec)
	body := rec.Body.String()

	for _, want := range []string{
		"kenwea_mcp_proposal_tokens_minted_total 2",
		"kenwea_mcp_proposal_tokens_redeemed_total 1",
		"kenwea_mcp_proposal_tokens_rejected_total 1",
		`kenwea_mcp_proposal_redemption_age_total{bucket="under_1h"} 1`,
		// Unused buckets must still be present at zero so the series exist.
		`kenwea_mcp_proposal_redemption_age_total{bucket="over_3d"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n--- got ---\n%s", want, body)
		}
	}
}

// The point of binding the fingerprint: redemption must be able to say whether
// the human decided on the terms still in force, or on ones that have moved.
func TestRedemptionClassifiesTermsCurrentVersusStale(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	now := time.Now()
	token := mintProposalToken(now, "fp-at-mint")

	same := verifyProposalToken(token, now.Add(time.Hour), "fp-at-mint")
	if !same.Valid || same.TermsClass != termsClassCurrent {
		t.Fatalf("unchanged terms should classify as current, got %+v", same)
	}

	moved := verifyProposalToken(token, now.Add(time.Hour), "fp-after-change")
	if !moved.Valid {
		t.Fatal("a stale-terms token must still redeem — it is a category, not a rejection")
	}
	if moved.TermsClass != termsClassStale {
		t.Errorf("changed terms should classify as stale, got %q", moved.TermsClass)
	}
	if moved.MintedUnderFingerprint != "fp-at-mint" {
		t.Errorf("must report which terms it was minted under, got %q", moved.MintedUnderFingerprint)
	}
}

// Legacy 4-part tokens were promised a 7-day life before terms binding existed.
// Breaking that to tidy the format would be exactly the small dishonesty this
// mechanism exists to avoid.
func TestLegacyTokenStillRedeemsAsUnknownTerms(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	now := time.Now()
	// Hand-build the pre-binding format: prefix.unix.nonce.signature
	payload := "1785100000.bm9uY2U"
	legacy := proposalTokenPrefix + "." + payload + "." + signProposalPayload(proposalSecret(), payload)

	result := verifyProposalToken(legacy, time.Unix(1785100000, 0).Add(time.Hour), "fp-current")
	if !result.Valid {
		t.Fatalf("legacy token must still redeem: %s", result.Reason)
	}
	if result.TermsClass != termsClassUnknown {
		t.Errorf("legacy token has no binding, expected unknown, got %q", result.TermsClass)
	}
	_ = now
}

// The fingerprint is inside the signed payload, so swapping it must break the
// signature — otherwise "stale" could be forged into "current".
func TestBoundFingerprintCannotBeSwapped(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	token := mintProposalToken(time.Now(), "fp-real")
	tampered := strings.Replace(token, "fp-real", "fp-fake", 1)

	if r := verifyProposalToken(tampered, time.Now(), "fp-fake"); r.Valid {
		t.Fatal("a token with a swapped fingerprint must fail signature verification")
	}
}

func TestMetricsExposesTermsClassCounters(t *testing.T) {
	original := proposalStats
	proposalStats = &proposalCounters{ageBuckets: map[string]uint64{}, termsClasses: map[string]uint64{}}
	defer func() { proposalStats = original }()

	proposalStats.recordRedemption(10*time.Minute, termsClassCurrent)
	proposalStats.recordRedemption(48*time.Hour, termsClassStale)

	rec := httptest.NewRecorder()
	writeMCPMetrics(rec)
	body := rec.Body.String()

	for _, want := range []string{
		`kenwea_mcp_proposal_redemption_terms_total{class="current"} 1`,
		`kenwea_mcp_proposal_redemption_terms_total{class="stale"} 1`,
		// Present at zero so the series exists before the first legacy redemption.
		`kenwea_mcp_proposal_redemption_terms_total{class="unknown"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n--- got ---\n%s", want, body)
		}
	}
}

// The descriptor's fingerprint and the one bound into its token must agree —
// if they drifted, every install would look stale-termed from birth.
func TestDescriptorTokenIsBoundToTheFingerprintItPublishes(t *testing.T) {
	t.Setenv("KENWEA_PROPOSAL_TOKEN_SECRET", "test-secret")
	body := fetchDescriptor(t)
	published, _ := body["termsFingerprint"].(string)
	proposal, _ := body["installProposal"].(map[string]any)
	attribution, _ := proposal["attribution"].(map[string]any)
	token, _ := attribution["token"].(string)

	result := verifyProposalToken(token, time.Now(), published)
	if !result.Valid {
		t.Fatalf("descriptor's own token failed verification: %s", result.Reason)
	}
	if result.TermsClass != termsClassCurrent {
		t.Fatalf("a token redeemed immediately must be current, got %q (minted under %q, published %q)",
			result.TermsClass, result.MintedUnderFingerprint, published)
	}
}
