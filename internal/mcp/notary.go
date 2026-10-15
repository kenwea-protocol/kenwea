package mcp

// The notary server: kenwea.sandbox.check offered on its own, at /notary/v1.
//
// Why a second endpoint rather than a smaller list on the first. Decided
// 2026-09-29 with the operator. The one capability with value today is the
// notary, and the market that exists for it is developers and CI asking what a
// package does at install (see the notary pivot, 2026-08-10). The main server
// leads with 28 marketplace tools that such a caller does not want, and it asks
// for a key before the first useful call. This endpoint has four tools, needs no
// key, and is listed on its own. Glama scored the main server's tool count 2/5
// and suggested exactly this split.
//
// What it shares with the main server: the process, the protocol helpers, the
// platform forwarder and the platform's rate limits. What it deliberately does
// not share: sessions, the tourist gate and the marketplace tool list. Only the
// four tools below exist here, so nothing else can be reached through it.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

const (
	notaryPath       = "/notary/v1"
	notaryCheckTool  = "kenwea.notary.check"
	notaryVerifyTool = "kenwea.notary.verify"
	notaryKeyTool    = "kenwea.notary.getPublicKey"
	notaryTreeTool   = "kenwea.notary.checkTree"
)

// The names tools/list offers. Until 2026-10-11 the list offered the dotted names
// above; VS Code refuses a tool whose name holds anything but [a-z0-9_-] ("Tool
// names may only contain [a-z0-9_-]"), so Copilot agent mode could see the three
// tools and use none. The dotted names still answer, as unlisted aliases: the
// published CLI, the GitHub Action and existing configs call them.
const (
	notaryCheckName  = "kenwea_notary_check"
	notaryVerifyName = "kenwea_notary_verify"
	notaryKeyName    = "kenwea_notary_get_public_key"
	notaryTreeName   = "kenwea_notary_check_tree"
)

// notaryTool maps a called name, listed or alias, to the tool it means.
var notaryTool = map[string]string{
	notaryCheckName: notaryCheckTool, notaryCheckTool: notaryCheckTool,
	notaryVerifyName: notaryVerifyTool, notaryVerifyTool: notaryVerifyTool,
	notaryKeyName: notaryKeyTool, notaryKeyTool: notaryKeyTool,
	notaryTreeName: notaryTreeTool, notaryTreeTool: notaryTreeTool,
}

// attestationKeyURL is where the notary's public key is published (the same URL
// every signed record names). A var so tests can serve their own key.
var attestationKeyURL = "https://www.kenwea.com/.well-known/kenwea-attestation-key"

// notaryInstructions is what a client reads when it connects. Until 2026-10-11
// it ended with a pointer to the marketplace; that came out for the Claude
// connector directory, whose policy does not let a server steer the model
// toward other tools or services the user did not ask for. The notary
// describes itself and nothing else, in its introduction and in every result.
const notaryInstructions = "Kenwea notary. kenwea_notary_check fetches an https file, npm tarball or Python wheel (or an npm package by name), runs it with no network, and returns a verdict signed under a published Ed25519 key and bound to the sha256 of the bytes. kenwea_notary_verify checks such a signed record, and kenwea_notary_get_public_key returns the key so you can check it yourself. No key, signup or payment: checks are limited per network address and in total per hour. A signed record states only what the artifact did."

func notaryServerInfo() map[string]string {
	return map[string]string{"name": "kenwea-notary", "title": "Kenwea Notary", "version": "1.0.0"}
}

func (s *Server) serveNotary(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == notaryPath+"/health" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "server": "kenwea-notary"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeHTTPError(w, http.StatusMethodNotAllowed, nil, "method_not_allowed", "the notary endpoint takes JSON-RPC over POST; it keeps no sessions and opens no stream")
		return
	}
	if !allowedOrigin(r.Header.Get("Origin")) {
		writeHTTPError(w, http.StatusForbidden, nil, "origin_rejected", "origin is not allowed for public MCP")
		return
	}
	body, err := io.ReadAll(io.LimitReader(http.MaxBytesReader(w, r.Body, maxRequestBytes), maxRequestBytes))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, nil, "invalid_json", "could not read request body")
		return
	}
	if firstJSONToken(body) == '[' {
		writeRPCError(w, http.StatusOK, nil, "batch_not_supported", "this server takes one JSON-RPC request per POST; send the requests separately")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, nil, "invalid_json", "invalid JSON-RPC request")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcErrorObj{Code: -32600, Message: "Invalid Request", Data: map[string]string{"detail": "a request must carry jsonrpc:\"2.0\" and a method"}}})
		return
	}
	if req.ID == nil {
		// A notification: nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	meta := readRequestMeta(req.Params)
	stateless := isStatelessRequest(r, req, meta)
	if stateless {
		if !meta.hasVersion || !meta.hasCapabilities {
			writeProtocolError(w, http.StatusBadRequest, req.ID, codeInvalidParams, "Invalid params", map[string]any{
				"detail": "a request without initialize must carry its protocol version and client capabilities in params._meta",
			})
			return
		}
		if meta.protocolVersion != ProtocolStateless {
			writeUnsupportedVersion(w, req.ID, meta.protocolVersion)
			return
		}
		if detail := headerMismatch(r, req, meta); detail != "" {
			writeProtocolError(w, http.StatusBadRequest, req.ID, codeHeaderMismatch, "Header mismatch", map[string]string{"detail": detail})
			return
		}
		req.Params = withoutMeta(req.Params)
	} else if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedProtocolVersions[v] {
		writeUnsupportedVersion(w, req.ID, v)
		return
	}

	reply := func(result map[string]any) {
		if stateless {
			result = notaryComplete(result)
		}
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
	}
	switch req.Method {
	case "initialize":
		reply(map[string]any{
			"protocolVersion": legacyVersionFor(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      notaryServerInfo(),
			"instructions":    notaryInstructions,
		})
	case "ping":
		reply(map[string]any{})
	case "server/discover":
		reply(map[string]any{
			"supportedVersions": SupportedProtocolVersionList(),
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      notaryInstructions,
		})
	case "tools/list":
		reply(map[string]any{"tools": notaryToolDescriptors()})
	case "tools/call":
		name, args, err := decodeMCPToolCall(req.Params)
		if err != nil {
			writeProtocolError(w, http.StatusOK, req.ID, codeInvalidParams, "Invalid params", map[string]string{"detail": err.Error()})
			return
		}
		if stateless {
			args = withoutMeta(args)
		}
		switch notaryTool[name] {
		case notaryCheckTool:
			reply(s.notaryCheck(r, args))
		case notaryVerifyTool:
			reply(notaryVerify(args))
		case notaryKeyTool:
			reply(notaryPublicKey())
		case notaryTreeTool:
			reply(s.notaryTree(r, args))
		default:
			writeRPCMethodNotFound(w, req.ID, "the notary server has four tools, kenwea_notary_check, kenwea_notary_check_tree, kenwea_notary_verify and kenwea_notary_get_public_key; the marketplace tools are at /mcp/v1")
		}
	default:
		writeRPCMethodNotFound(w, req.ID, "this server implements the tools capability only")
	}
}

// notaryComplete marks a stateless result complete and names this server, not
// the marketplace one.
func notaryComplete(result map[string]any) map[string]any {
	out := completeResult(result)
	if meta, ok := out["_meta"].(map[string]any); ok {
		meta[metaServerInfo] = notaryServerInfo()
	}
	return out
}

// toolError is a tool result the caller can act on: the call reached the tool
// and the tool declined, which MCP reports as a result with isError set rather
// than as a protocol failure.
func toolError(code, detail string) map[string]any {
	body := map[string]any{"error": code, "detail": detail}
	payload, _ := json.Marshal(body)
	return map[string]any{
		"content":           []map[string]string{{"type": "text", "text": string(payload)}},
		"structuredContent": body,
		"isError":           true,
	}
}

func (s *Server) notaryCheck(r *http.Request, args json.RawMessage) map[string]any {
	var in struct {
		ArtifactRef string `json:"artifactRef"`
		Package     string `json:"package"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolError("validation_failed", "arguments must be an object with artifactRef or package")
	}
	ref, pkg := strings.TrimSpace(in.ArtifactRef), strings.TrimSpace(in.Package)
	if (ref == "") == (pkg == "") {
		return toolError("validation_failed", "give exactly one of artifactRef (an https URL) or package (an npm package name such as express@4.18.2, or pypi:requests@2.31.0)")
	}
	keyed := strings.TrimSpace(r.Header.Get("Authorization")) != ""
	// Logged so the first real external caller is visible the day it happens,
	// which is the test this server exists to run. No arguments are logged, and
	// the caller appears only as a short hash of its address: enough to tell our
	// own test calls from someone else's, not enough to identify anyone.
	log.Printf("mcp.notary.call tool=%s keyed=%t by_package=%t client=%s", notaryCheckTool, keyed, pkg != "", callerHash(r))
	if s.forwarder == nil {
		return toolError("platform_api_unavailable", "the notary is not connected to the platform")
	}
	forwarded, _ := json.Marshal(map[string]string{"artifactRef": ref, "package": pkg})
	result, err := s.forwarder.ForwardTool(r, notaryCheckTool, forwarded)
	if err != nil {
		var pe *PlatformError
		if errors.As(err, &pe) && pe.StatusCode >= 400 && pe.StatusCode < 500 {
			detail := pe.Detail
			if pe.StatusCode == http.StatusUnauthorized {
				detail = "the Authorization header was not a valid Kenwea key; send no Authorization to check without a key"
			}
			return toolError(pe.Code, detail)
		}
		return toolError("platform_api_unavailable", "the platform did not answer; retry later")
	}
	return responseResult(result, true).(map[string]any)
}

// notaryTree answers for a whole dependency tree from stored records
// (2026-10-15). It forwards the lockfile or the package list as given; the
// platform parses it, answers what it holds and queues the rest.
func (s *Server) notaryTree(r *http.Request, args json.RawMessage) map[string]any {
	var in struct {
		Lockfile json.RawMessage  `json:"lockfile"`
		Packages []map[string]any `json:"packages"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolError("validation_failed", "arguments must be an object with lockfile (the text of a package-lock.json, pnpm-lock.yaml or yarn.lock) or packages")
	}
	if len(in.Lockfile) == 0 && len(in.Packages) == 0 {
		return toolError("validation_failed", "give lockfile (the text of a package-lock.json v2 or v3, a pnpm-lock.yaml or a yarn.lock) or packages ([{name, version, hasInstallScript}])")
	}
	log.Printf("mcp.notary.call tool=%s packages=%d lockfile=%t client=%s", notaryTreeTool, len(in.Packages), len(in.Lockfile) > 0, callerHash(r))
	if s.forwarder == nil {
		return toolError("platform_api_unavailable", "the notary is not connected to the platform")
	}
	result, err := s.forwarder.ForwardTool(r, notaryTreeTool, args)
	if err != nil {
		var pe *PlatformError
		if errors.As(err, &pe) && pe.StatusCode >= 400 && pe.StatusCode < 500 {
			return toolError(pe.Code, pe.Detail)
		}
		return toolError("platform_api_unavailable", "the platform did not answer; retry later")
	}
	return responseResult(result, true).(map[string]any)
}

// publishedKey is one entry of the published key list.
type publishedKey struct {
	ID     string
	Pub    ed25519.PublicKey
	Status string // active, retired or revoked
	Reason string
}

type notaryKeys struct {
	mu       sync.Mutex
	keys     []publishedKey
	fallback bool      // keys is the single PEM key, read because the list was not
	expires  time.Time // when the cached keys must be read again
	retry    time.Time // before this, a record naming an unknown key does not trigger a read
}

func hasKey(keys []publishedKey, id string) bool {
	for _, k := range keys {
		if k.ID == id {
			return true
		}
	}
	return false
}

var publishedKeys notaryKeys

// attestationKeysURL lists every key Kenwea has signed with (2026-10-08), so a
// record stays checkable after a rotation and one signed with a revoked key is
// reported as such. A var so tests can serve their own list.
var attestationKeysURL = "https://www.kenwea.com/.well-known/kenwea-attestation-keys.json"

var notaryHTTP = &http.Client{Timeout: 10 * time.Second}

// keyIDOf is the key id every record names: the first 16 hex characters of the
// SHA-256 of the base64 public key.
func keyIDOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(base64.StdEncoding.EncodeToString(pub)))
	return hex.EncodeToString(sum[:])[:16]
}

// list returns the published keys. The list is cached for an hour, but a
// record naming a key the cache does not hold reads it again, at most once a
// minute, so a rotation counts at once rather than an hour late. A server that
// has not published a list falls back to the single PEM key, taken as active
// and cached for five minutes only, so a list that appears is not ignored for
// an hour. A list read once and unreachable later is kept, because it still
// carries every revocation, and read again a minute later.
func (k *notaryKeys) list(wanted string) ([]publishedKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	if k.keys != nil && now.Before(k.expires) && (wanted == "" || hasKey(k.keys, wanted) || now.Before(k.retry)) {
		return k.keys, nil
	}
	keys, err := fetchKeyList()
	switch {
	case err == nil:
		k.keys, k.fallback, k.expires = keys, false, now.Add(time.Hour)
	case k.keys != nil && !k.fallback:
		k.expires = now.Add(time.Minute)
	default:
		pub, perr := fetchPEMKey()
		if perr != nil {
			return nil, fmt.Errorf("%v; and %v", err, perr)
		}
		k.keys, k.fallback, k.expires = []publishedKey{{ID: keyIDOf(pub), Pub: pub, Status: "active"}}, true, now.Add(5*time.Minute)
	}
	k.retry = now.Add(time.Minute)
	return k.keys, nil
}

func fetchKeyList() ([]publishedKey, error) {
	resp, err := notaryHTTP.Get(attestationKeysURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key list returned HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			KeyID           string `json:"keyId"`
			Algorithm       string `json:"algorithm"`
			PublicKeyBase64 string `json:"publicKeyBase64"`
			Status          string `json:"status"`
			Reason          string `json:"reason"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("key list is not readable JSON: %v", err)
	}
	var keys []publishedKey
	for _, entry := range doc.Keys {
		raw, err := base64.StdEncoding.DecodeString(entry.PublicKeyBase64)
		if err != nil || len(raw) != ed25519.PublicKeySize || entry.Algorithm != "ed25519" {
			continue
		}
		pub := ed25519.PublicKey(raw)
		// The id is recomputed, never taken from the list, so a list entry
		// cannot claim another key's id.
		if entry.KeyID != keyIDOf(pub) {
			continue
		}
		keys = append(keys, publishedKey{ID: entry.KeyID, Pub: pub, Status: entry.Status, Reason: entry.Reason})
	}
	if len(keys) == 0 {
		return nil, errors.New("key list holds no usable Ed25519 key")
	}
	return keys, nil
}

func fetchPEMKey() (ed25519.PublicKey, error) {
	resp, err := notaryHTTP.Get(attestationKeyURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key endpoint returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("key endpoint did not return a PEM key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("published key is not Ed25519")
	}
	return pub, nil
}

func activeKey(keys []publishedKey) (publishedKey, bool) {
	for _, k := range keys {
		if k.Status == "active" {
			return k, true
		}
	}
	return publishedKey{}, false
}

func notaryVerify(args json.RawMessage) map[string]any {
	var in struct {
		Payload       string `json:"payload"`
		Signature     string `json:"signature"`
		ContentSHA256 string `json:"contentSha256"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Payload == "" || in.Signature == "" {
		return toolError("validation_failed", "payload and signature are required, exactly as they appear in the record's signedAttestation")
	}
	var facts map[string]any
	_ = json.Unmarshal([]byte(in.Payload), &facts)
	// A format 2 record names its key inside the signed payload; an older one
	// does not, and is tried against every published key.
	wanted, _ := facts["keyId"].(string)
	keys, err := publishedKeys.list(wanted)
	if err != nil {
		return toolError("key_unavailable", "could not fetch the published keys from "+attestationKeysURL+": "+err.Error())
	}
	sig, sigErr := base64.StdEncoding.DecodeString(strings.TrimSpace(in.Signature))
	var signer *publishedKey
	if sigErr == nil {
		for i := range keys {
			if wanted != "" && keys[i].ID != wanted {
				continue
			}
			if ed25519.Verify(keys[i].Pub, []byte(in.Payload), sig) {
				signer = &keys[i]
				break
			}
		}
	}
	result := map[string]any{"keysUrl": attestationKeysURL}
	if signer == nil {
		result["valid"] = false
		result["reason"] = "the signature does not match this payload under any key Kenwea has published: the record was altered, re-serialised, or signed by someone else"
		if wanted != "" && !hasKey(keys, wanted) {
			result["reason"] = "the record names key " + wanted + ", which is not in Kenwea's published key list"
		}
		return responseResult(result, true).(map[string]any)
	}
	result["keyId"], result["keyStatus"] = signer.ID, signer.Status
	if signer.Status == "revoked" {
		result["valid"] = false
		result["reason"] = "the signature is Kenwea's, but the key that made it was revoked, so the record cannot be trusted: " + signer.Reason
		return responseResult(result, true).(map[string]any)
	}
	result["valid"] = true
	if facts != nil {
		result["facts"] = facts
	}
	result["log"] = checkInLog(in.Payload, signer.Pub)
	if want := strings.ToLower(strings.TrimSpace(in.ContentSHA256)); want != "" {
		got, _ := facts["contentSha256"].(string)
		result["matchesContentSha256"] = strings.EqualFold(got, want)
	}
	return responseResult(result, true).(map[string]any)
}

func notaryToolDescriptors() []map[string]any {
	return []map[string]any{
		{
			"name":        notaryCheckName,
			"title":       "Notarize what an artifact does",
			"description": "Notarize what a file or npm package does at the moment Kenwea fetches it, and get a signed record anyone can check.\nInput: exactly one of artifactRef, a public https URL of a single file, npm tarball or Python wheel, or package, an npm package name such as express@4.18.2 (resolved to the exact tarball npm install would download; no version means latest), or a Python one as pypi:requests@2.31.0 (resolved on PyPI to the wheel this sandbox can import: pure Python, or musllinux for CPython 3.12).\nBehavior: Kenwea downloads the bytes (up to 32 MiB) and runs executable content in isolation: no network, all capabilities dropped, read-only filesystem, not as root, 15 seconds for a file and 55 for a package. For a package it runs the install steps npm would run (preinstall, install, postinstall, and the node-gyp step npm adds for a binding.gyp) with decoy credentials where a developer machine or CI runner keeps real ones (an npm token in ~/.npmrc and NPM_TOKEN, a GitHub token, AWS keys, an SSH key, CI variables), each traced for the network connections, DNS lookups and programs it attempts, the credential files it opens and any decoy it puts into what it sends or starts; a single file and a Python package run with the same decoys (plus a PyPI token for Python) and are traced the same way; a PyPI wheel (package pypi:name) is imported with its dependencies installed, resolved as pip would for CPython 3.12 on Linux x86_64 and each checked against PyPI's sha256. After its install a registry npm package is also loaded once, as an application would require or import it, and onLoad says what loading it did; a load that reaches the network or a credential is its own finding (load_tried_network, load_read_credentials, load_sent_credentials). For a registry package that runs something at install, the dependency tree npm would install goes in first, each tarball checked against the registry's integrity hash, and every install step in the tree runs, the dependencies' first; a package that declares none is checked when a dependency does (dependencies.installSteps names them).\nReturns: level, one word for what the record means (nothing_runs, quiet, network, reads_credentials, sends_credentials, unfinished), and summary, one sentence; installSteps (what runs at install; empty means nothing does) and observed (what the steps attempted, with credentialsRead and credentialsSent when a decoy was touched); for an npm package, sincePrevious, which install steps were added, removed or changed since the previous published version (from the registry, not signed); a verdict and a reasonCode, the sha256 of what was read, and signedAttestation, an Ed25519 signature over the facts. quiet (verdict approved) means everything that ran finished, was traced whole and touched nothing watched; a step that tried the network, read a credential file, failed or could not be observed is manual_review with the reason; rejected is for a step that tried to send a decoy credential, and for files carrying a provider-formatted credential. A URL that cannot be fetched returns checked false with the reason instead of a verdict; a limit of our runner comes back as manual_review stated as ours.\nLimits: no key or signup; 20 checks per hour per network address (an IPv6 /64 counts as one address) within a shared hourly ceiling, refused with rate_limited and the reset time. A Kenwea API key sent as a Bearer token uses that key's own quota. At most four checks run at once; one that gets no slot within 10 seconds comes back manual_review with reasonCode runner_busy and nothing run, so retry. The same bytes at the same address under the same checker version get the record issued the first time, marked cached, and nothing is run again. Keeps no copy of the artifact; a finished record is held in memory for up to 24 hours to answer repeats, and a rate counter and a log line keep a short hash of your address.\nNot for: checking a record you already have (use kenwea_notary_verify, which runs nothing and does not spend your quota).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"artifactRef": map[string]any{"type": "string", "format": "uri", "description": "Public https URL of the artifact: a single .js, .mjs, .cjs or .py file, a shebang script, an npm tarball (.tgz) or a Python wheel or zip. Omit when using package."},
					"package":     map[string]any{"type": "string", "description": "npm package name with an optional version or dist-tag, for example express, express@4.18.2 or @types/node@20.0.0; or a PyPI project as pypi:name or pypi:name@version, for example pypi:requests@2.31.0. Omit when using artifactRef."},
				},
			},
			"annotations": map[string]any{"title": "Notarize what an artifact does", "readOnlyHint": true, "destructiveHint": false, "idempotentHint": false, "openWorldHint": true},
		},
		{
			"name":        notaryVerifyName,
			"title":       "Verify a signed Kenwea record",
			"description": "Check a signed record produced by kenwea_notary_check: whether its signature is valid under Kenwea's published Ed25519 key, what it attests, and whether it is in Kenwea's public transparency log (log.included, proven with an inclusion proof against a checkpoint signed by the same key), so it was shown to everyone and not only to you.\nInput: payload and signature exactly as they appear in the record's signedAttestation. payload is a JSON string and must be passed byte for byte; re-serialising it (reordering keys, changing spacing) breaks the signature. signature is standard base64 with padding. Optionally contentSha256, the hex sha256 of bytes you hold, compared without regard to letter case.\nBehavior: runs nothing and makes no request except fetching Kenwea's published key list (https://www.kenwea.com/.well-known/kenwea-attestation-keys.json), which it caches for an hour and reads again when a record names a key it does not hold. A record names its key inside the signed payload (format 2) and is checked against that key; an older record is tried against every published key. A record signed with a key Kenwea has revoked returns valid false and says so. Read-only and idempotent; it never counts against the check quota.\nReturns: valid, the keyId and its status, and the signed facts (verdict, reasonCode, installSteps, observed, contentSha256, issuedAt and the rest); with contentSha256, also matchesContentSha256. An altered or re-serialised record returns valid false with the reason, not an error. If the key list cannot be fetched the call fails with key_unavailable and says why.\nNot for: learning what an artifact does (use kenwea_notary_check). To verify without this tool, take the key from kenwea_notary_get_public_key and use any Ed25519 library.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"payload", "signature"},
				"properties": map[string]any{
					"payload":       map[string]any{"type": "string", "description": "signedAttestation.payload from a check result, byte for byte."},
					"signature":     map[string]any{"type": "string", "description": "signedAttestation.signature, base64."},
					"contentSha256": map[string]any{"type": "string", "description": "Optional sha256 (hex) of the bytes you hold; the answer then says whether the record is about them."},
				},
			},
			"annotations": map[string]any{"title": "Verify a signed Kenwea record", "readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		},
		{
			"name":        notaryKeyName,
			"title":       "Get the notary's public key",
			"description": "Return Kenwea's published Ed25519 notary key, so a signed record can be verified with your own code instead of kenwea_notary_verify.\nInput: none.\nBehavior: reads Kenwea's published key list (https://www.kenwea.com/.well-known/kenwea-attestation-keys.json) and caches it for an hour. Runs nothing, read-only, never counts against the check quota. Fails with key_unavailable if the list cannot be fetched.\nReturns: the active key's keyId, algorithm ed25519, the key as base64 (32 raw bytes) and as PEM, keyUrl and keysUrl, and keys, every published key with its status (active, retired or revoked). To verify, check signedAttestation.signature (base64) over the exact bytes of signedAttestation.payload with the key whose keyId the payload names, and treat a revoked key as no signature.\nNot for: checking an artifact (use kenwea_notary_check) or having the check done for you (use kenwea_notary_verify).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			"annotations": map[string]any{"title": "Get the notary's public key", "readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		},
		{
			"name":        notaryTreeName,
			"title":       "Check a whole dependency tree",
			"description": "Answer for every package in a project's npm dependency tree at once, from records the notary already holds.\nInput: lockfile, the text of a package-lock.json (lockfileVersion 2 or 3, or its JSON), a pnpm-lock.yaml (v5, v6, v9) or a yarn.lock (v1 or berry); or packages, a list of {name, version, hasInstallScript}.\nBehavior: the lockfile says which packages run something at install (hasInstallScript, or requiresBuild in pnpm up to v6), and where it does not (pnpm v9, yarn) the registry is asked for each version (usually a few percent run something); the rest run nothing of their own and are only counted. Each package that runs something is answered from its stored signed record, made by kenwea_notary_check's sandbox run with decoy credentials; one that has none yet is queued and checked in the background, about two a minute, so ask again later with the same tree to collect it. Nothing runs during the call and it does not spend the per-address check quota (60 tree calls an hour per address).\nReturns: summary, counts (total, runNothing, runSomething, answered, queued), findings (packages whose level is network, reads_credentials or sends_credentials), and packages, each with status (answered, queued, not_queued) and, when answered, level, summary, verdict, reasonCode, observed and signedAttestation. Verify each record with kenwea_notary_verify or your own Ed25519 code; the tree summary itself is not signed.\nNot for: one package you want checked now (use kenwea_notary_check). A lockfileVersion 1 file has no hasInstallScript flags and is refused with a reason.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lockfile": map[string]any{"description": "The contents of a package-lock.json (lockfileVersion 2 or 3, as text or as the JSON object), a pnpm-lock.yaml or a yarn.lock, as text."},
					"packages": map[string]any{"type": "array", "description": "Alternatively, the installed packages.", "items": map[string]any{"type": "object", "properties": map[string]any{
						"name": map[string]any{"type": "string"}, "version": map[string]any{"type": "string"}, "hasInstallScript": map[string]any{"type": "boolean"}}}},
				},
			},
			"annotations": map[string]any{"title": "Check a whole dependency tree", "readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true},
		},
	}
}

// callerHash is the first 12 hex characters of the SHA-256 of the caller's
// address, the same prefix the platform's rate buckets use.
func callerHash(r *http.Request) string {
	addr := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if addr == "" {
		addr = r.RemoteAddr
		if host, _, err := net.SplitHostPort(addr); err == nil {
			addr = host
		}
	}
	sum := sha256.Sum256([]byte(addr))
	return hex.EncodeToString(sum[:])[:12]
}

// notaryPublicKey returns the published key in the forms a verifier needs, so
// verification can be done with the caller's own code rather than by asking us.
func notaryPublicKey() map[string]any {
	keys, err := publishedKeys.list("")
	if err != nil {
		return toolError("key_unavailable", "could not fetch the published keys from "+attestationKeysURL+": "+err.Error())
	}
	active, ok := activeKey(keys)
	if !ok {
		return toolError("key_unavailable", "the published key list names no active key")
	}
	der, err := x509.MarshalPKIXPublicKey(active.Pub)
	if err != nil {
		return toolError("key_unavailable", "could not encode the published key: "+err.Error())
	}
	var history []map[string]any
	for _, k := range keys {
		entry := map[string]any{"keyId": k.ID, "status": k.Status, "publicKeyBase64": base64.StdEncoding.EncodeToString(k.Pub)}
		if k.Reason != "" {
			entry["reason"] = k.Reason
		}
		history = append(history, entry)
	}
	return responseResult(map[string]any{
		"keyId":           active.ID,
		"algorithm":       "ed25519",
		"publicKeyBase64": base64.StdEncoding.EncodeToString(active.Pub),
		"publicKeyPem":    string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		"keyUrl":          attestationKeyURL,
		"keysUrl":         attestationKeysURL,
		"keys":            history,
	}, true).(map[string]any)
}

// The transparency log (2026-10-15): a record the notary keeps is appended to a
// public Merkle log whose head is a checkpoint signed with the same key. The
// verifier checks the checkpoint against Kenwea's published key, not against
// anything the API says about itself, then checks the record's inclusion with
// Go's own tlog. A record that is not in the log is not invalid, but it was not
// shown to everyone.
var notaryLogBase = "https://api.kenwea.com/public/notary/log"

const notaryLogOrigin = "kenwea.com/notary-log"

func logVerifierKey(pub ed25519.PublicKey) string {
	key := append([]byte{1}, pub...)
	h := sha256.New()
	h.Write([]byte(notaryLogOrigin + "\n"))
	h.Write(key)
	return fmt.Sprintf("%s+%08x+%s", notaryLogOrigin, binary.BigEndian.Uint32(h.Sum(nil)), base64.StdEncoding.EncodeToString(key))
}

func getLogJSON(url string, into any) error {
	resp, err := notaryHTTP.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotInLog
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env); err != nil {
		return err
	}
	return json.Unmarshal(env.Data, into)
}

var errNotInLog = errors.New("not in the log")

func b64Hash(s string) (tlog.Hash, error) {
	var h tlog.Hash
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != len(h) {
		return h, fmt.Errorf("bad hash %q", s)
	}
	copy(h[:], b)
	return h, nil
}

// checkInLog proves payload is in the log under a checkpoint signed by pub.
func checkInLog(payload string, pub ed25519.PublicKey) map[string]any {
	out := map[string]any{"checkpointUrl": notaryLogBase + "/checkpoint"}
	fail := func(reason string) map[string]any { out["included"], out["reason"] = false, reason; return out }
	v, err := note.NewVerifier(logVerifierKey(pub))
	if err != nil {
		return fail("could not build the checkpoint key: " + err.Error())
	}
	var cp struct {
		Checkpoint string `json:"checkpoint"`
	}
	if err := getLogJSON(notaryLogBase+"/checkpoint", &cp); err != nil {
		return fail("could not fetch the checkpoint: " + err.Error())
	}
	n, err := note.Open([]byte(cp.Checkpoint), note.VerifierList(v))
	if err != nil {
		return fail("the checkpoint is not signed by the key that signed this record")
	}
	lines := strings.Split(n.Text, "\n")
	if len(lines) < 3 || lines[0] != notaryLogOrigin {
		return fail("the checkpoint is not the notary log's")
	}
	size, err := strconv.ParseInt(lines[1], 10, 64)
	root, err2 := b64Hash(lines[2])
	if err != nil || err2 != nil {
		return fail("the checkpoint cannot be read")
	}
	sum := sha256.Sum256([]byte(payload))
	var proof struct {
		Index int64    `json:"index"`
		Proof []string `json:"proof"`
	}
	if err := getLogJSON(fmt.Sprintf("%s/proof?sha=%x&size=%d", notaryLogBase, sum, size), &proof); err != nil {
		if errors.Is(err, errNotInLog) {
			return fail("this record is not in the public log: it was not kept (for example a check of an arbitrary URL), or it was made after the checkpoint")
		}
		return fail("could not fetch the proof: " + err.Error())
	}
	var hs []tlog.Hash
	for _, p := range proof.Proof {
		h, err := b64Hash(p)
		if err != nil {
			return fail("the proof cannot be read")
		}
		hs = append(hs, h)
	}
	if err := tlog.CheckRecord(hs, size, root, proof.Index, tlog.RecordHash([]byte(payload))); err != nil {
		return fail("the inclusion proof does not check against the signed checkpoint: " + err.Error())
	}
	out["included"], out["index"], out["size"] = true, proof.Index, size
	return out
}
