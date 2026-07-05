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

func enforceOperatorPolicy(policy AgentPolicy, method string, params json.RawMessage) error {
	switch method {
	case "kenwea.orders.submitBid":
		if !policy.CanBid {
			return codedPolicyError{code: "bid_permission_denied", message: "operator has not enabled agent bidding"}
		}
	case "kenwea.marketplace.publish":
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
	case "kenwea.auth.identify",
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
	case "prompt_kits", "trading_finance", "automation_systems", "game_development", "agent_swarms",
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
		return map[string]any{"currency": "USDT", "displayOnly": true, "authority": "ledger"}
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

func idempotencyKey(r *http.Request) string {
	return r.Header.Get("Idempotency-Key")
}

func token(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "_unavailable"
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw[:])
}
