package mcp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type notaryReply struct {
	Result map[string]any `json:"result"`
	Error  map[string]any `json:"error"`
}

func callNotary(t *testing.T, server http.Handler, method string, params string, headers map[string]string) (int, notaryReply) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
	req := httptest.NewRequest(http.MethodPost, notaryPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	var out notaryReply
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func structured(t *testing.T, r notaryReply) (map[string]any, bool) {
	t.Helper()
	sc, _ := r.Result["structuredContent"].(map[string]any)
	isErr, _ := r.Result["isError"].(bool)
	return sc, isErr
}

// Exactly three tools, on both protocol eras, and nothing from the marketplace can
// be reached through this path.
func TestNotaryListsThreeToolsAndReachesNothingElse(t *testing.T) {
	forwarder := &recordingForwarder{}
	server := NewServer(StaticAuthenticator{})
	server.forwarder = forwarder

	_, init := callNotary(t, server, "initialize", `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`, nil)
	if info, _ := init.Result["serverInfo"].(map[string]any); info["name"] != "kenwea-notary" || init.Result["instructions"] == "" {
		t.Fatalf("initialize should name the notary server: %v", init.Result)
	}

	stateless := `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`
	for _, era := range []struct {
		params  string
		headers map[string]string
	}{
		{`{}`, map[string]string{"MCP-Protocol-Version": "2025-11-25"}},
		{stateless, map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list"}},
	} {
		_, list := callNotary(t, server, "tools/list", era.params, era.headers)
		tools, _ := list.Result["tools"].([]any)
		if len(tools) != 3 {
			t.Fatalf("expected 3 notary tools, got %d: %v", len(tools), list)
		}
	}

	for _, name := range []string{"kenwea.marketplace.search", "kenwea.sandbox.check", "kenwea.onboarding.registerSelf"} {
		_, out := callNotary(t, server, "tools/call", `{"name":"`+name+`","arguments":{}}`, nil)
		if out.Error == nil || out.Error["code"].(float64) != -32601 {
			t.Fatalf("%s must not be callable on the notary path, got %v", name, out)
		}
	}
	if forwarder.method != "" {
		t.Fatalf("nothing should have been forwarded, got %s", forwarder.method)
	}
}

func TestNotaryCheckForwardsWithoutAKeyAndValidatesFirst(t *testing.T) {
	forwarder := &recordingForwarder{result: map[string]any{"checked": true, "verdict": "approved"}}
	server := NewServer(StaticAuthenticator{})
	server.forwarder = forwarder

	for _, args := range []string{`{}`, `{"artifactRef":"https://example.com/a.js","package":"express"}`} {
		_, out := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.check","arguments":`+args+`}`, nil)
		if sc, isErr := structured(t, out); !isErr || sc["error"] != "validation_failed" {
			t.Fatalf("%s: expected a validation_failed tool error, got %v", args, out)
		}
	}
	if forwarder.method != "" {
		t.Fatalf("an invalid call must not be forwarded")
	}

	_, out := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.check","arguments":{"package":"express@4.18.2"}}`, nil)
	sc, isErr := structured(t, out)
	if isErr || sc["verdict"] != "approved" {
		t.Fatalf("a keyless check should come back with the platform's verdict, got %v", out)
	}
	if forwarder.method != notaryCheckTool || !strings.Contains(string(forwarder.params), `"package":"express@4.18.2"`) {
		t.Fatalf("forwarded %s %s", forwarder.method, forwarder.params)
	}
}

type notaryFailingForwarder struct{ err error }

func (f notaryFailingForwarder) ForwardTool(*http.Request, string, json.RawMessage) (map[string]any, error) {
	return nil, f.err
}

func TestNotaryCheckReportsAPlatformRefusalAsAToolError(t *testing.T) {
	server := NewServer(StaticAuthenticator{})
	server.forwarder = notaryFailingForwarder{&PlatformError{StatusCode: http.StatusTooManyRequests, Code: "rate_limited", Detail: "limited; resets in 60 seconds"}}
	_, out := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.check","arguments":{"artifactRef":"https://example.com/a.js"}}`, nil)
	if sc, isErr := structured(t, out); !isErr || sc["error"] != "rate_limited" || !strings.Contains(sc["detail"].(string), "resets") {
		t.Fatalf("expected the platform's rate_limited as a tool error, got %v", out)
	}
}

func TestNotaryVerifyChecksTheSignatureAgainstThePublishedKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = pem.Encode(w, &pem.Block{Type: "PUBLIC KEY", Bytes: der})
	}))
	defer keyServer.Close()
	oldURL := attestationKeyURL
	attestationKeyURL = keyServer.URL
	oldKeysURL := attestationKeysURL
	attestationKeysURL = keyServer.URL + "/keys.json"
	defer func() { attestationKeysURL = oldKeysURL }()
	publishedKeys = notaryKeys{}
	defer func() { attestationKeyURL = oldURL; publishedKeys = notaryKeys{} }()

	payload := `{"issuer":"kenwea.com","contentSha256":"abc123","verdict":"approved"}`
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))
	server := NewServer(StaticAuthenticator{})
	verify := func(p, s, sha string) map[string]any {
		args, _ := json.Marshal(map[string]string{"payload": p, "signature": s, "contentSha256": sha})
		_, out := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.verify","arguments":`+string(args)+`}`, nil)
		sc, isErr := structured(t, out)
		if isErr {
			t.Fatalf("verify returned a tool error: %v", out)
		}
		return sc
	}

	good := verify(payload, sig, "ABC123")
	facts, _ := good["facts"].(map[string]any)
	if good["valid"] != true || facts["verdict"] != "approved" || good["matchesContentSha256"] != true {
		t.Fatalf("a genuine record should verify and match its hash: %v", good)
	}
	if other := verify(payload, sig, "def456"); other["valid"] != true || other["matchesContentSha256"] != false {
		t.Fatalf("a genuine record about other bytes: %v", other)
	}
	altered := strings.Replace(payload, "approved", "rejected", 1)
	if bad := verify(altered, sig, ""); bad["valid"] != false || bad["facts"] != nil {
		t.Fatalf("an altered record must not verify or report facts: %v", bad)
	}
	reserialised := strings.Replace(payload, ",", ", ", 1)
	if bad := verify(reserialised, sig, ""); bad["valid"] != false {
		t.Fatalf("a re-serialised payload must not verify: %v", bad)
	}
}

// The main endpoint's list is unchanged by the notary: its tools live only here.
func TestNotaryToolsAreNotOnTheMainList(t *testing.T) {
	for _, tool := range mcpToolDescriptors() {
		if name := tool["name"].(string); strings.HasPrefix(name, "kenwea.notary.") {
			t.Fatalf("%s leaked into the main tools/list", name)
		}
	}
	if allowedTool(notaryCheckTool) || allowedTool(notaryVerifyTool) {
		t.Fatalf("notary tools must not be callable on /mcp/v1")
	}
}

// The notary's one pointer to the marketplace lives in its introduction, never
// in a check result: a verdict carrying a sales pitch would stop reading as a
// neutral third party's.
func TestMarketplacePointerIsInTheIntroductionNotInResults(t *testing.T) {
	forwarder := &recordingForwarder{result: map[string]any{"checked": true, "verdict": "approved"}}
	server := NewServer(StaticAuthenticator{})
	server.forwarder = forwarder

	_, init := callNotary(t, server, "initialize", `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`, nil)
	if !strings.Contains(init.Result["instructions"].(string), "kenwea.onboarding.registerSelf") {
		t.Fatalf("the introduction should say how a maker can list what it checked")
	}
	_, out := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.check","arguments":{"package":"left-pad@1.3.0"}}`, nil)
	raw, _ := json.Marshal(out.Result)
	for _, pitch := range []string{"registerSelf", "marketplace", "offer it", "sale"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(pitch)) {
			t.Fatalf("a check result must not carry the marketplace pointer (%q): %s", pitch, raw)
		}
	}
}

// getPublicKey hands out the same key verify uses, in forms ordinary Ed25519
// code accepts, so a record can be checked without asking us to check it.
func TestNotaryPublicKeyVerifiesARecordWithoutUs(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = pem.Encode(w, &pem.Block{Type: "PUBLIC KEY", Bytes: der})
	}))
	oldURL := attestationKeyURL
	attestationKeyURL = keyServer.URL
	oldKeysURL := attestationKeysURL
	attestationKeysURL = keyServer.URL + "/keys.json"
	defer func() { attestationKeysURL = oldKeysURL }()
	publishedKeys = notaryKeys{}
	defer func() { attestationKeyURL = oldURL; publishedKeys = notaryKeys{} }()
	server := NewServer(StaticAuthenticator{})

	_, out := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.getPublicKey","arguments":{}}`, nil)
	key, isErr := structured(t, out)
	if isErr {
		t.Fatalf("getPublicKey failed: %v", out)
	}
	raw, err := base64.StdEncoding.DecodeString(key["publicKeyBase64"].(string))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		t.Fatalf("publicKeyBase64 is not a 32-byte key: %v %d", err, len(raw))
	}
	block, _ := pem.Decode([]byte(key["publicKeyPem"].(string)))
	if block == nil {
		t.Fatalf("publicKeyPem does not parse")
	}
	payload := `{"verdict":"approved"}`
	sig := ed25519.Sign(priv, []byte(payload))
	if !ed25519.Verify(ed25519.PublicKey(raw), []byte(payload), sig) {
		t.Fatalf("a record does not verify under the returned key")
	}
	args, _ := json.Marshal(map[string]string{"payload": payload, "signature": base64.StdEncoding.EncodeToString(sig)})
	_, v := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.verify","arguments":`+string(args)+`}`, nil)
	verified, _ := structured(t, v)
	if verified["keyId"] != key["keyId"] || verified["valid"] != true {
		t.Fatalf("verify and getPublicKey disagree: %v vs %v", verified, key)
	}

	keyServer.Close()
	publishedKeys = notaryKeys{}
	_, gone := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.getPublicKey","arguments":{}}`, nil)
	if sc, isErr := structured(t, gone); !isErr || sc["error"] != "key_unavailable" {
		t.Fatalf("an unreachable key must be key_unavailable, got %v", gone)
	}
}

// After a rotation (2026-10-08) records are checked against the published key
// list: one signed with the active key verifies, one signed with a revoked key
// is reported as revoked rather than valid, and a format 2 record is checked
// only against the key it names.
func TestNotaryVerifyFollowsTheKeyList(t *testing.T) {
	activePub, activePriv, _ := ed25519.GenerateKey(rand.Reader)
	oldPub, oldPriv, _ := ed25519.GenerateKey(rand.Reader)
	list, _ := json.Marshal(map[string]any{"keys": []map[string]any{
		{"keyId": keyIDOf(activePub), "algorithm": "ed25519", "publicKeyBase64": base64.StdEncoding.EncodeToString(activePub), "status": "active"},
		{"keyId": keyIDOf(oldPub), "algorithm": "ed25519", "publicKeyBase64": base64.StdEncoding.EncodeToString(oldPub), "status": "revoked", "reason": "stored where it could have been copied"},
		// An entry claiming the active key's id for another key is ignored.
		{"keyId": keyIDOf(activePub), "algorithm": "ed25519", "publicKeyBase64": base64.StdEncoding.EncodeToString(oldPub), "status": "active"},
	}})
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(list) }))
	defer keyServer.Close()
	oldKeysURL := attestationKeysURL
	attestationKeysURL = keyServer.URL
	publishedKeys = notaryKeys{}
	defer func() { attestationKeysURL = oldKeysURL; publishedKeys = notaryKeys{} }()

	server := NewServer(StaticAuthenticator{})
	verify := func(payload string, priv ed25519.PrivateKey) map[string]any {
		args, _ := json.Marshal(map[string]string{"payload": payload, "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
		_, out := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.verify","arguments":`+string(args)+`}`, nil)
		sc, isErr := structured(t, out)
		if isErr {
			t.Fatalf("verify returned a tool error: %v", out)
		}
		return sc
	}
	current := `{"issuer":"kenwea.com","format":2,"keyId":"` + keyIDOf(activePub) + `","verdict":"approved"}`
	if got := verify(current, activePriv); got["valid"] != true || got["keyStatus"] != "active" {
		t.Fatalf("a record signed with the active key must verify: %v", got)
	}
	legacy := `{"issuer":"kenwea.com","verdict":"approved"}`
	if got := verify(legacy, oldPriv); got["valid"] != false || got["keyStatus"] != "revoked" || !strings.Contains(got["reason"].(string), "revoked") {
		t.Fatalf("a record signed with a revoked key must say so, not verify: %v", got)
	}
	if got := verify(legacy, activePriv); got["valid"] != true {
		t.Fatalf("a format 1 record is tried against every published key: %v", got)
	}
	misnamed := `{"issuer":"kenwea.com","format":2,"keyId":"` + keyIDOf(oldPub) + `","verdict":"approved"}`
	if got := verify(misnamed, activePriv); got["valid"] != false {
		t.Fatalf("a format 2 record is checked only against the key it names: %v", got)
	}
}

// A rotation counts as soon as a record names the new key, not an hour later
// when the cache would have expired; and a list read once is kept when it
// becomes unreachable, so a revocation does not lapse with the network.
func TestNotaryKeyCacheFollowsARotation(t *testing.T) {
	oldPub, oldPriv, _ := ed25519.GenerateKey(rand.Reader)
	newPub, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	entry := func(pub ed25519.PublicKey, status string) map[string]any {
		return map[string]any{"keyId": keyIDOf(pub), "algorithm": "ed25519", "publicKeyBase64": base64.StdEncoding.EncodeToString(pub), "status": status}
	}
	before, _ := json.Marshal(map[string]any{"keys": []map[string]any{entry(oldPub, "active")}})
	after, _ := json.Marshal(map[string]any{"keys": []map[string]any{entry(newPub, "active"), entry(oldPub, "revoked")}})
	var mu sync.Mutex
	serve, down := before, false
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(serve)
	}))
	defer keyServer.Close()
	oldKeysURL, oldKeyURL := attestationKeysURL, attestationKeyURL
	attestationKeysURL, attestationKeyURL = keyServer.URL, keyServer.URL
	publishedKeys = notaryKeys{}
	defer func() { attestationKeysURL, attestationKeyURL = oldKeysURL, oldKeyURL; publishedKeys = notaryKeys{} }()

	server := NewServer(StaticAuthenticator{})
	verify := func(payload string, priv ed25519.PrivateKey) map[string]any {
		args, _ := json.Marshal(map[string]string{"payload": payload, "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
		_, out := callNotary(t, server, "tools/call", `{"name":"kenwea.notary.verify","arguments":`+string(args)+`}`, nil)
		sc, isErr := structured(t, out)
		if isErr {
			t.Fatalf("verify returned a tool error: %v", out)
		}
		return sc
	}
	oldRecord := `{"issuer":"kenwea.com","format":2,"keyId":"` + keyIDOf(oldPub) + `","verdict":"approved"}`
	newRecord := `{"issuer":"kenwea.com","format":2,"keyId":"` + keyIDOf(newPub) + `","verdict":"approved"}`
	if got := verify(oldRecord, oldPriv); got["valid"] != true {
		t.Fatalf("before the rotation the old key is active: %v", got)
	}

	mu.Lock()
	serve = after
	mu.Unlock()
	// Within the minute, an unknown key does not trigger a read, and the
	// answer names the key rather than blaming the payload.
	if got := verify(newRecord, newPriv); got["valid"] != false || !strings.Contains(got["reason"].(string), keyIDOf(newPub)) {
		t.Fatalf("an unknown key must be named: %v", got)
	}
	publishedKeys.mu.Lock()
	publishedKeys.retry = time.Time{} // a minute later
	publishedKeys.mu.Unlock()
	if got := verify(newRecord, newPriv); got["valid"] != true {
		t.Fatalf("a record naming the new key must read the list again and verify: %v", got)
	}
	if got := verify(oldRecord, oldPriv); got["valid"] != false || got["keyStatus"] != "revoked" {
		t.Fatalf("after the rotation the old key is revoked: %v", got)
	}

	mu.Lock()
	down = true
	mu.Unlock()
	publishedKeys.mu.Lock()
	publishedKeys.expires = time.Time{} // the hour is up and the list is unreachable
	publishedKeys.mu.Unlock()
	if got := verify(oldRecord, oldPriv); got["valid"] != false || got["keyStatus"] != "revoked" {
		t.Fatalf("a revocation must survive the list becoming unreachable: %v", got)
	}
}
