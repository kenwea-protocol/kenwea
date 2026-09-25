package platformapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// kenwea.marketplace.search declares seven parameters in its MCP schema. This is the
// test that the declaration is true: before 2026-07-30 route() returned a bare
// "/products" and every argument was discarded, so a caller asking for "trading tools
// under $10, newest first" got the same unfiltered first page as a caller asking for
// nothing -- with no error to indicate its question had been thrown away.
func TestSearchForwardsEveryDeclaredParameter(t *testing.T) {
	params := json.RawMessage(`{"q":"trading bot","category":"trading_finance","minPriceCents":100,"maxPriceCents":5000,"sort":"price_asc","limit":25,"offset":50}`)
	method, path, _, err := route("kenwea.marketplace.search", params)
	if err != nil {
		t.Fatalf("route returned an error: %v", err)
	}
	if method != http.MethodGet {
		t.Fatalf("expected GET, got %s", method)
	}
	for _, want := range []string{
		"q=trading+bot",
		"category=trading_finance",
		"minPriceCents=100",
		"maxPriceCents=5000",
		"sort=price_asc",
		"limit=25",
		"offset=50",
	} {
		if !strings.Contains(path, want) {
			t.Errorf("path %q is missing %q", path, want)
		}
	}
}

// An argument outside the declared seven must not reach the platform. Otherwise the
// schema would be a description of the polite path rather than of the surface.
func TestSearchDropsUndeclaredParameters(t *testing.T) {
	params := json.RawMessage(`{"q":"bot","sellerId":"agent_other","internalOnly":true}`)
	_, path, _, err := route("kenwea.marketplace.search", params)
	if err != nil {
		t.Fatalf("route returned an error: %v", err)
	}
	if strings.Contains(path, "sellerId") || strings.Contains(path, "internalOnly") {
		t.Fatalf("undeclared parameters were forwarded: %s", path)
	}
}

// Zero is a value a caller may mean, so it has to survive. `minPriceCents: 0` and an
// absent minPriceCents are the same filter on the platform side, but only because the
// handler treats 0 as "no bound" -- the transport must not be the thing that decides
// that, or a later change to the handler would silently change the tool's meaning.
func TestSearchDistinguishesZeroFromAbsent(t *testing.T) {
	_, withZero, _, err := route("kenwea.marketplace.search", json.RawMessage(`{"minPriceCents":0}`))
	if err != nil {
		t.Fatalf("route returned an error: %v", err)
	}
	if !strings.Contains(withZero, "minPriceCents=0") {
		t.Fatalf("an explicit zero was dropped: %q", withZero)
	}
	_, withNothing, _, err := route("kenwea.marketplace.search", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("route returned an error: %v", err)
	}
	if withNothing != "/products" {
		t.Fatalf("an empty argument object should produce no query string, got %q", withNothing)
	}
}

func TestSearchToleratesMissingAndMalformedParams(t *testing.T) {
	for _, params := range []string{``, `null`, `{`, `{"q":123}`} {
		_, path, _, err := route("kenwea.marketplace.search", json.RawMessage(params))
		if err != nil {
			t.Fatalf("params %q: route returned an error: %v", params, err)
		}
		if path != "/products" {
			t.Fatalf("params %q: expected a bare /products, got %q", params, path)
		}
	}
}

// The body a tool forwards must still carry its own arguments. Adding a query string
// to search must not have changed anything for the POST tools.
func TestPublishStillForwardsItsBody(t *testing.T) {
	params := json.RawMessage(`{"title":"Tool","version":"1.0.0"}`)
	method, path, body, err := route("kenwea.marketplace.publish", params)
	if err != nil {
		t.Fatalf("route returned an error: %v", err)
	}
	if method != http.MethodPost || path != "/agent/products/publish" {
		t.Fatalf("unexpected route: %s %s", method, path)
	}
	if body == nil {
		t.Fatal("publish forwarded a nil body")
	}
	forwarded, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading forwarded body: %v", err)
	}
	if string(forwarded) != string(params) {
		t.Fatalf("forwarded body %q does not match params %q", forwarded, params)
	}
}
