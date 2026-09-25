package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

var allowedTools = map[string]struct{}{
	"kenwea.onboarding.startOperatorAgent":   {},
	"kenwea.onboarding.registerSelf":         {},
	"kenwea.auth.identify":                   {},
	"kenwea.auth.profile":                    {},
	"kenwea.agent.identity":                  {},
	"kenwea.agent.heartbeat":                 {},
	"kenwea.marketplace.search":              {},
	"kenwea.marketplace.preview":             {},
	"kenwea.marketplace.publish":             {},
	"kenwea.marketplace.purchase":            {},
	"kenwea.marketplace.install":             {},
	"kenwea.wallet.balance":                  {},
	"kenwea.wallet.transactions":             {},
	"kenwea.notifications.list":              {},
	"kenwea.notifications.ack":               {},
	"kenwea.jobs.getStatus":                  {},
	"kenwea.sandbox.check":                   {},
	"kenwea.orders.listRequests":             {},
	"kenwea.orders.submitBid":                {},
	"kenwea.orders.deliver":                  {},
	"kenwea.collab.create":                   {},
	"kenwea.collab.join":                     {},
	"kenwea.procurement.memory":              {},
	"kenwea.reputation.graph":                {},
	"kenwea.community.ask":                   {},
	"kenwea.observer.feed":                   {},
	"kenwea.analytics.forecast":              {},
	"kenwea.recommendations.relatedProducts": {},
	"kenwea.dependencies.watch":              {},
	"kenwea.scale.status":                    {},
}

var idempotentTools = map[string]struct{}{
	"kenwea.marketplace.publish":           {},
	"kenwea.marketplace.purchase":          {},
	"kenwea.marketplace.install":           {},
	"kenwea.notifications.ack":             {},
	"kenwea.orders.submitBid":              {},
	"kenwea.orders.deliver":                {},
	"kenwea.collab.create":                 {},
	"kenwea.collab.join":                   {},
	"kenwea.dependencies.watch":            {},
	"kenwea.onboarding.startOperatorAgent": {},
}

// mutatingTools is every tool the PRD classifies as a write/economic action. It is
// deliberately broader than idempotentTools: it also covers writes (like community.ask)
// that must force fresh Authorization to close a session-replay revocation gap, even
// though they don't need an Idempotency-Key.
var mutatingTools = map[string]struct{}{
	"kenwea.marketplace.publish":           {},
	"kenwea.marketplace.purchase":          {},
	"kenwea.marketplace.install":           {},
	"kenwea.notifications.ack":             {},
	"kenwea.orders.submitBid":              {},
	"kenwea.orders.deliver":                {},
	"kenwea.collab.create":                 {},
	"kenwea.collab.join":                   {},
	"kenwea.dependencies.watch":            {},
	"kenwea.onboarding.startOperatorAgent": {},
	"kenwea.community.ask":                 {},
	"kenwea.sandbox.check":                 {},
	// kenwea.agent.heartbeat is intentionally excluded: it's a low-stakes, non-destructive
	// liveness ping, not an economic or destructive action, so a revoked-but-cached
	// session replaying it carries no meaningful risk.
}

type AgentPolicy struct {
	CanBid              bool `json:"canBid"`
	CanPublish          bool `json:"canPublish"`
	AllowDynamicPricing bool `json:"allowDynamicPricing"`
}

const operatorRequiredMessage = "Action forbidden: Unbound Agent. Please provide your unique Agent ID to your Operator and ask them to claim your account and configure your permissions via the Operator Control Plane."

func allowedTool(method string) bool {
	_, ok := allowedTools[method]
	return ok
}

func enforceOperatorPolicy(policy AgentPolicy, method string, params json.RawMessage, unclaimed bool) error {
	switch method {
	case "kenwea.orders.submitBid":
		if !policy.CanBid {
			return codedPolicyError{code: "bid_permission_denied", message: "operator has not enabled agent bidding"}
		}
	case "kenwea.marketplace.publish":
		// An unclaimed agent has no operator, so it has no operator permissions to
		// check. Both gates below exist to stop an agent exceeding what its
		// operator delegated; there is nothing to exceed, and the platform side
		// caps what an unclaimed publish can become regardless.
		if unclaimed {
			return nil
		}
		if !policy.CanPublish {
			return codedPolicyError{code: "publish_permission_denied", message: "operator has not enabled product publishing"}
		}
		if publishRequestsDynamicPricing(params) && !policy.AllowDynamicPricing {
			return codedPolicyError{code: "dynamic_pricing_denied", message: "operator has not delegated dynamic pricing"}
		}
	}
	return nil
}

func rejectUnboundMutatingAgent(actor Actor, method string) error {
	if actor.Type != "agent" || actor.OperatorID != "" || touristAllowedTool(method) {
		return nil
	}
	return errors.New(operatorRequiredMessage)
}

func touristAllowedTool(method string) bool {
	switch method {
	// community.ask is the one write a tourist may perform. A visiting agent
	// could already read the whole market loop (search, request board,
	// reputation, observer feed) but had no way to report what it did NOT
	// find -- "why is there no X here?" -- because every feedback channel
	// required an operator binding it had not made yet. That inverted the
	// point of an open door: the gaps a newcomer notices are exactly the
	// signal worth collecting, and it is lost if only bound sellers can speak.
	// Safe to allow because the platform side moderates and persists it:
	// assistant.ValidateQuestion rejects before CreateAssistantQuestion runs,
	// and the payload is structured rather than free-form broadcast.
	// Budgeted on the platform side rather than here, so an caller hitting the
	// API directly cannot skip it: 10/hour per actor and 30/hour per client
	// address. The client dimension is the load-bearing one -- self-registration
	// is capped per address, but without it one address could still mint free
	// keys and multiply a per-actor limit by however many it made.
	case
		// Publishing is open to an unclaimed agent as of 2026-07-31, and it is the
		// change that turns "there is nothing for me here" into a reason to stay.
		// Measured the same day: 15 external sources read the full tool list in 24
		// hours and none went further, because everything that makes this a
		// marketplace sat behind a human the agent had not met.
		//
		// What it can reach is the whole path -- validation, the sandbox, a real
		// verdict on its artifact. What it cannot reach is a buyer: migration
		// 000038 refuses to let a listing from an unclaimed agent go live, in the
		// database rather than here. So A2.1 -- every economic action is
		// attributable to an operator -- stays literally true, because a draft
		// nobody can buy is not an economic action.
		"kenwea.marketplace.publish",
		// And the tool that tells it what happened. Publishing returns a job id and
		// names kenwea.jobs.getStatus as the way to follow it; without this line
		// that call answered operator_required, so an unclaimed agent could publish
		// and then never learn its verdict. Found on 2026-07-31 by walking the path
		// as an outside agent immediately after deploying it -- a state with no
		// exit, shipped by the same hand that spent the day removing three others.
		//
		// Safe to open because the platform now scopes a job read to the actor that
		// enqueued it. Before that fix the route took no credential at all, so this
		// line would have handed every tourist a reader for anyone's publish
		// payload.
		"kenwea.jobs.getStatus",
		// The sandbox, offered on its own terms.
		//
		// Measured 2026-08-06: ~1,000 requests a day and not one tool call from
		// anyone but us. Being listed in three directories, which we now are, does
		// not change what an arriving agent FINDS -- six listings it does not want
		// and a selling path that needs a human it has not met. Every other
		// capability here is worth something only once the market has liquidity.
		// This one is worth something on the first call, to an agent with no
		// intention of selling anything, and it was reachable only by publishing a
		// product first.
		//
		// What it offers is not execution -- agents can run code. It is a
		// third-party attestation, which an agent cannot produce for itself
		// because that is circular.
		"kenwea.sandbox.check",
		"kenwea.community.ask",
		"kenwea.auth.identify",
		"kenwea.auth.profile",
		"kenwea.agent.identity",
		"kenwea.agent.heartbeat",
		"kenwea.marketplace.search",
		"kenwea.orders.listRequests",
		"kenwea.procurement.memory",
		"kenwea.reputation.graph",
		"kenwea.observer.feed",
		"kenwea.analytics.forecast",
		"kenwea.recommendations.relatedProducts",
		"kenwea.scale.status":
		return true
	default:
		return false
	}
}

func publishRequestsDynamicPricing(params json.RawMessage) bool {
	var body struct {
		AllowDynamicPricing bool `json:"allowDynamicPricing"`
	}
	_ = json.Unmarshal(params, &body)
	return body.AllowDynamicPricing
}

// publishSourceFramework extracts the optional, non-authorizing sourceFramework
// telemetry field from a publish payload. It returns a sanitized value (printable,
// at most 64 runes) or "" when the field is absent or unusable. It never rejects a
// publish: telemetry must never gate an economic action, and a crafted value must
// never be able to corrupt a log line.
func publishSourceFramework(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var body struct {
		SourceFramework string `json:"sourceFramework"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return ""
	}
	value := strings.TrimSpace(body.SourceFramework)
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if runes := []rune(value); len(runes) > 64 {
		value = string(runes[:64])
	}
	return value
}

type codedPolicyError struct {
	code    string
	message string
}

func (e codedPolicyError) Error() string {
	return e.message
}

func policyCode(err error) string {
	if coded, ok := err.(codedPolicyError); ok {
		return coded.code
	}
	return "permission_denied"
}

func requiresIdempotency(method string) bool {
	_, ok := idempotentTools[method]
	return ok
}

func requiresMutating(method string) bool {
	_, ok := mutatingTools[method]
	return ok
}

func forwardsToPlatform(method string) bool {
	switch method {
	case "kenwea.marketplace.search",
		"kenwea.onboarding.registerSelf",
		"kenwea.onboarding.startOperatorAgent",
		"kenwea.agent.heartbeat",
		"kenwea.marketplace.preview",
		"kenwea.marketplace.publish",
		"kenwea.marketplace.purchase",
		"kenwea.marketplace.install",
		"kenwea.wallet.balance",
		"kenwea.wallet.transactions",
		"kenwea.notifications.list",
		"kenwea.notifications.ack",
		"kenwea.jobs.getStatus",
		"kenwea.sandbox.check",
		"kenwea.orders.listRequests",
		"kenwea.orders.submitBid",
		"kenwea.orders.deliver",
		"kenwea.collab.create",
		"kenwea.collab.join",
		"kenwea.procurement.memory",
		"kenwea.reputation.graph",
		"kenwea.community.ask",
		"kenwea.observer.feed",
		"kenwea.analytics.forecast",
		"kenwea.recommendations.relatedProducts",
		"kenwea.dependencies.watch",
		"kenwea.scale.status":
		return true
	default:
		return false
	}
}

func lowPriorityTool(method string) bool {
	switch method {
	case "kenwea.observer.feed",
		"kenwea.analytics.forecast",
		"kenwea.recommendations.relatedProducts",
		"kenwea.scale.status":
		return true
	default:
		return false
	}
}

func validateToolParams(method string, params json.RawMessage) error {
	if method != "kenwea.marketplace.publish" {
		return nil
	}
	var body struct {
		Title     string `json:"title"`
		Version   string `json:"version"`
		Summary   string `json:"summary"`
		Category  string `json:"category"`
		License   string `json:"license"`
		Artifact  string `json:"artifactRef"`
		Agreement bool   `json:"sellerAgreementAccepted"`
		Images    []struct {
			URL     string `json:"url"`
			AltText string `json:"altText"`
		} `json:"images"`
	}
	if len(params) == 0 || string(params) == "null" {
		return errors.New("publish requires product image assets")
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return errors.New("invalid publish params")
	}
	if strings.TrimSpace(body.Title) == "" || strings.TrimSpace(body.Version) == "" || strings.TrimSpace(body.Summary) == "" || strings.TrimSpace(body.Category) == "" || strings.TrimSpace(body.License) == "" || strings.TrimSpace(body.Artifact) == "" {
		return errors.New("publish requires title, version, summary, category, license, and artifactRef")
	}
	if !allowedMarketplaceCategory(body.Category) {
		return errors.New("publish category must be an approved digital product category")
	}
	if !body.Agreement {
		return errors.New("publish requires accepted seller agreement")
	}
	if len(body.Images) == 0 {
		return errors.New("publish requires at least one product image")
	}
	for _, image := range body.Images {
		if strings.TrimSpace(image.URL) == "" || strings.TrimSpace(image.AltText) == "" {
			return errors.New("each product image requires url and altText")
		}
		if !strings.HasPrefix(image.URL, "https://") && !strings.HasPrefix(image.URL, "r2://") && !strings.HasPrefix(image.URL, "/assets/") {
			return errors.New("product image url must be https, r2, or committed asset path")
		}
	}
	return nil
}

func allowedMarketplaceCategory(value string) bool {
	switch strings.TrimSpace(value) {
	case "prompt_kits", "trading_finance", "web3_crypto", "ecommerce_stores", "automation_systems", "game_development", "agent_swarms",
		"code_modules", "saas_starters", "security_audit", "data_research",
		"design_media_assets", "3d_game_architecture", "marketing_sales", "business_templates", "education_training",
		"capability", "automation", "game_assets", "game_tools", "data_intelligence", "security_ops",
		"agents_personas", "design_media", "media_assets", "3d_assets", "cad_assets", "autocad", "architecture_assets":
		return true
	default:
		return false
	}
}

func requestHash(params json.RawMessage) string {
	sum := sha256.Sum256(params)
	return hex.EncodeToString(sum[:])
}

func resultFor(method string, actor Actor) map[string]any {
	switch method {
	case "kenwea.auth.identify", "kenwea.auth.profile", "kenwea.agent.identity":
		return map[string]any{"actor": actor, "phase": "phase_2_marketplace"}
	case "kenwea.agent.heartbeat":
		return map[string]any{"status": "accepted", "actor": actor}
	case "kenwea.marketplace.preview", "kenwea.marketplace.publish":
		return asyncJobEnvelope(method)
	case "kenwea.marketplace.purchase", "kenwea.marketplace.install":
		return map[string]any{"status": "forwarded_to_platform_api", "actor": actor, "authority": "platform_api"}
	case "kenwea.marketplace.search":
		return map[string]any{"products": []any{}, "sandboxGate": "required", "authority": "platform_api"}
	case "kenwea.wallet.balance":
		// Machine-readable balance terms, mirrored from the platform wallet reads:
		// an agent must learn "spend-only, no withdrawal in v1" from the tool
		// result itself, not from human-facing FAQ prose.
		return map[string]any{"currency": "USDT", "displayOnly": true, "authority": "ledger", "terms": map[string]any{"unspentBalancePolicy": "spend_only", "withdrawal": "none_v1", "expiry": "none"}}
	case "kenwea.wallet.transactions":
		return map[string]any{"transactions": []any{}, "authority": "ledger"}
	case "kenwea.notifications.list":
		return map[string]any{"notifications": []any{}, "structuredOnly": true}
	case "kenwea.notifications.ack":
		return map[string]any{"status": "ack_forwarded", "authority": "platform_api"}
	case "kenwea.jobs.getStatus":
		return map[string]any{"status": "queued", "statusTool": "kenwea.jobs.getStatus"}
	case "kenwea.orders.listRequests":
		return map[string]any{"requests": []any{}, "authority": "platform_api"}
	case "kenwea.orders.submitBid", "kenwea.orders.deliver", "kenwea.collab.create", "kenwea.collab.join":
		return map[string]any{"status": "forwarded_to_platform_api", "actor": actor, "authority": "platform_api"}
	case "kenwea.procurement.memory":
		return map[string]any{"entries": []any{}, "privacy": "structured_records_only", "authority": "platform_api"}
	case "kenwea.reputation.graph":
		return map[string]any{"edges": []any{}, "dimensions": reputationDimensions(), "authority": "platform_api"}
	case "kenwea.community.ask":
		return map[string]any{"status": "moderated_forward", "structuredOnly": true, "authority": "platform_api"}
	case "kenwea.observer.feed":
		return map[string]any{"items": []any{}, "publicSafe": true, "authority": "platform_api"}
	case "kenwea.analytics.forecast":
		return map[string]any{"reports": []any{}, "advisoryOnly": true, "authority": "platform_api"}
	case "kenwea.recommendations.relatedProducts":
		return map[string]any{"edges": []any{}, "explainable": true, "authority": "platform_api"}
	case "kenwea.dependencies.watch":
		return map[string]any{"status": "watch_forwarded", "idempotent": true, "authority": "platform_api"}
	case "kenwea.scale.status":
		return map[string]any{"backpressure": "low_priority_shed_first", "authority": "platform_api"}
	case "kenwea.onboarding.startOperatorAgent":
		return map[string]any{"status": "forward_to_platform_api", "actor": actor}
	case "kenwea.onboarding.registerSelf":
		return map[string]any{"status": "forwarded_to_platform_api", "touristMode": true}
	default:
		return map[string]any{"status": "unsupported"}
	}
}

func reputationDimensions() []string {
	return []string{"delivery_speed", "buyer_return_rate", "sandbox_pass_rate", "dispute_rate", "niche_expertise", "referral_weight", "collab_reliability"}
}

func asyncJobEnvelope(method string) map[string]any {
	return map[string]any{
		"jobId":      token("job"),
		"traceId":    token("trace"),
		"tool":       method,
		"status":     "queued",
		"statusTool": "kenwea.jobs.getStatus",
		"poll":       map[string]any{"intervalSeconds": 5, "maxAttempts": 60},
		"sseEvent":   "job.succeeded",
		"authority":  "platform_api",
	}
}

// idempotencyKey resolves the retry key for a call, preferring the HTTP header and
// falling back to an `idempotencyKey` argument.
//
// The header alone was not enough, and the gap was total rather than partial: the MCP
// tools/call envelope carries a method and an arguments object, and gives a client no
// way to set an HTTP header per call. So every one of the ten tools in idempotentTools
// -- publish, purchase, install, submitBid, deliver, collab create/join, notifications
// ack, dependencies watch, startOperatorAgent -- was unreachable from a standards-
// compliant MCP client, which got `idempotency_required` on a header it had no means of
// sending. Every write on the platform, gated behind a mechanism only our own bridge
// could satisfy. The header remains the preferred form and existing callers are
// unaffected; the argument is the form an MCP client can actually produce.
func idempotencyKey(r *http.Request, params json.RawMessage) string {
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		return key
	}
	return IdempotencyKeyFromParams(params)
}

// IdempotencyKeyFromParams reads the `idempotencyKey` argument out of a tool call's
// params. Exported because the forwarding layer must reach the same verdict as the gate
// -- if the gate accepts a key from params and the forwarder only reads the header, the
// platform refuses the call the gate just let through.
func IdempotencyKeyFromParams(params json.RawMessage) string {
	if len(params) == 0 || string(params) == "null" {
		return ""
	}
	var body struct {
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.IdempotencyKey)
}

func token(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "_unavailable"
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw[:])
}
