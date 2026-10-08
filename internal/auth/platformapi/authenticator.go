package platformapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp"
)

type Authenticator struct {
	BaseURL string
	Client  *http.Client
	// ToolClient carries forwarded tool calls. It is separate from Client on
	// purpose: identifying a caller is a lookup that should be fast or fail,
	// while a forwarded tool may legitimately take a long time.
	ToolClient *http.Client
}

// authTimeout bounds /internal/mcp/identify. Five seconds is generous for a
// lookup and the right thing to give up on.
const authTimeout = 5 * time.Second

// toolTimeout bounds a forwarded tool call, and it is set by the slowest tool
// rather than the average one.
//
// This was 5s for everything, which silently broke kenwea.sandbox.check on any
// artifact that took longer than that: fetch, scan, base64, hand to the runner,
// docker run. Small files finished in time and large ones did not, so the tool
// looked size-limited when it was actually time-limited -- and the caller was
// told "platform api is temporarily unavailable", which blames our
// infrastructure for a budget we set. Measured 2026-08-08: left-pad at 1.4KB
// passed while Sentry's 1.58MB bundle failed, as did a 37KB package that was
// merely slow to execute.
//
// The sandbox's own budget is 15s for a file and 45s for a package, and the
// runner client allows another 10s on top, so this has to sit above that or the
// timeout fires in the wrong place and reports the wrong thing.
const toolTimeout = 90 * time.Second

func New(baseURL string) Authenticator {
	return Authenticator{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Client:     &http.Client{Timeout: authTimeout},
		ToolClient: &http.Client{Timeout: toolTimeout},
	}
}

func (a Authenticator) Authenticate(r *http.Request) (mcp.AuthResult, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.BaseURL+"/internal/mcp/identify", nil)
	if err != nil {
		return mcp.AuthResult{}, err
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
	setForwardedClient(req, r)
	resp, err := a.client().Do(req)
	if err != nil {
		return mcp.AuthResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&envelope)
		if envelope.Error.Code == "revoked_key" {
			return mcp.AuthResult{Revoked: true}, nil
		}
		return mcp.AuthResult{}, errors.New("platform api rejected MCP authentication")
	}
	if resp.StatusCode != http.StatusOK {
		return mcp.AuthResult{}, http.ErrNoCookie
	}
	var envelope struct {
		Data struct {
			Actor  mcp.Actor       `json:"actor"`
			Policy mcp.AgentPolicy `json:"policy"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return mcp.AuthResult{}, err
	}
	return mcp.AuthResult{Actor: envelope.Data.Actor, Policy: envelope.Data.Policy}, nil
}

func (a Authenticator) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

// toolClient is what forwarded tool calls use. Falling back to client() rather
// than http.DefaultClient keeps every existing test that only sets Client
// working, and keeps an unbounded default from creeping in.
func (a Authenticator) toolClient() *http.Client {
	if a.ToolClient != nil {
		return a.ToolClient
	}
	return a.client()
}

func (a Authenticator) ForwardTool(r *http.Request, method string, params json.RawMessage) (map[string]any, error) {
	httpMethod, path, body, err := route(method, params)
	if err != nil {
		return nil, &mcp.PlatformError{StatusCode: http.StatusBadRequest, Code: "validation_failed", Detail: err.Error()}
	}
	if method == "kenwea.notary.check" && strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		path = "/public/sandbox/check"
	}
	req, err := http.NewRequestWithContext(r.Context(), httpMethod, a.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
	setForwardedClient(req, r)
	// Must mirror mcp.idempotencyKey exactly: the gate in front of this accepts the
	// key from either the header or the params, so reading only the header here would
	// let a call past the gate and then have the platform refuse it for a missing key.
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = mcp.IdempotencyKeyFromParams(params)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.toolClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data  map[string]any `json:"data"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		// A non-JSON body (e.g. an nginx 5xx page) means we can't trust the
		// status as a structured platform verdict; surface it as an outage.
		return nil, fmt.Errorf("platform api returned an unreadable response for %s", method)
	}
	if resp.StatusCode >= 400 {
		code := envelope.Error.Code
		if code == "" {
			code = "platform_rejected"
		}
		return nil, &mcp.PlatformError{StatusCode: resp.StatusCode, Code: code, Detail: envelope.Error.Message}
	}
	return envelope.Data, nil
}

func route(method string, params json.RawMessage) (string, string, io.Reader, error) {
	switch method {
	case "kenwea.onboarding.registerSelf":
		return http.MethodPost, "/agent/self-registration", bytes.NewReader(params), nil
	case "kenwea.onboarding.startOperatorAgent":
		return http.MethodPost, "/operator/agents", bytes.NewReader(params), nil
	case "kenwea.agent.sendHeartbeat":
		return http.MethodPost, "/agent/heartbeat", nil, nil
	case "kenwea.marketplace.search":
		// The params were previously dropped on the floor: this returned "/products"
		// with no query string, so every search returned the same unfiltered first
		// page regardless of what the caller asked for. Not a rejection -- a silent
		// one, which is worse, because the caller gets a plausible answer to a
		// question it did not ask. GET /products reads q, category, minPriceCents,
		// maxPriceCents, sort, limit and offset; all seven now arrive.
		return http.MethodGet, "/products" + searchQuery(params), nil, nil
	case "kenwea.marketplace.preview":
		return http.MethodPost, "/agent/products/preview", bytes.NewReader(params), nil
	case "kenwea.marketplace.publish":
		return http.MethodPost, "/agent/products/publish", bytes.NewReader(params), nil
	case "kenwea.marketplace.purchase":
		return http.MethodPost, "/agent/purchases", bytes.NewReader(params), nil
	case "kenwea.marketplace.install":
		return http.MethodPost, "/agent/installations", bytes.NewReader(params), nil
	case "kenwea.wallet.getBalance":
		return http.MethodGet, "/agent/wallet", nil, nil
	case "kenwea.wallet.listTransactions":
		return http.MethodGet, "/agent/wallet/transactions", nil, nil
	case "kenwea.notifications.list":
		return http.MethodGet, "/agent/notifications", nil, nil
	case "kenwea.notifications.ack":
		id := paramValue(params, "notificationId")
		if id == "" {
			return "", "", nil, errors.New("notificationId is required")
		}
		return http.MethodPost, "/agent/notifications/" + url.PathEscape(id) + "/ack", nil, nil
	case "kenwea.jobs.getStatus":
		id := paramValue(params, "jobId")
		if id == "" {
			return "", "", nil, errors.New("jobId is required")
		}
		return http.MethodGet, "/agent/jobs/" + url.PathEscape(id), nil, nil
	case "kenwea.sandbox.check", "kenwea.notary.check":
		// kenwea.notary.check without a key goes to /public/sandbox/check instead;
		// ForwardTool decides, because only it can see the Authorization header.
		return http.MethodPost, "/agent/sandbox/check", bytes.NewReader(params), nil
	case "kenwea.orders.listRequests":
		return http.MethodGet, "/orders", nil, nil
	case "kenwea.orders.submitBid":
		id := paramValue(params, "requestId")
		if id == "" {
			return "", "", nil, errors.New("requestId is required")
		}
		return http.MethodPost, "/agent/orders/" + url.PathEscape(id) + "/bids", bytes.NewReader(params), nil
	case "kenwea.orders.deliver":
		id := paramValue(params, "milestoneId")
		if id == "" {
			return "", "", nil, errors.New("milestoneId is required")
		}
		return http.MethodPost, "/agent/milestones/" + url.PathEscape(id) + "/deliveries", bytes.NewReader(params), nil
	case "kenwea.collab.create":
		return http.MethodPost, "/agent/collabs", bytes.NewReader(params), nil
	case "kenwea.collab.join":
		id := paramValue(params, "collabId")
		if id == "" {
			return "", "", nil, errors.New("collabId is required")
		}
		return http.MethodPost, "/agent/collabs/" + url.PathEscape(id) + "/join", bytes.NewReader(params), nil
	case "kenwea.procurement.listDecisions":
		return http.MethodGet, "/agent/procurement", nil, nil
	case "kenwea.reputation.getGraph":
		id := paramValue(params, "agentId")
		if id == "" {
			return "", "", nil, errors.New("agentId is required")
		}
		return http.MethodGet, "/agents/" + url.PathEscape(id) + "/reputation", nil, nil
	case "kenwea.community.ask":
		return http.MethodPost, "/assistant/questions", bytes.NewReader(params), nil
	case "kenwea.observer.getFeed":
		cursor := paramValue(params, "cursor")
		if cursor != "" {
			return http.MethodGet, "/observer/feed?cursor=" + url.QueryEscape(cursor), nil, nil
		}
		return http.MethodGet, "/observer/feed", nil, nil
	case "kenwea.analytics.getForecast":
		return http.MethodGet, "/analytics/forecast", nil, nil
	case "kenwea.recommendations.listRelatedProducts":
		id := paramValue(params, "productId")
		if id == "" {
			return "", "", nil, errors.New("productId is required")
		}
		return http.MethodGet, "/products/" + url.PathEscape(id) + "/recommendations", nil, nil
	case "kenwea.dependencies.watch":
		id := paramValue(params, "productId")
		if id == "" {
			return "", "", nil, errors.New("productId is required")
		}
		return http.MethodPost, "/products/" + url.PathEscape(id) + "/dependencies/watch", bytes.NewReader(params), nil
	case "kenwea.scale.getStatus":
		return http.MethodGet, "/scale/status", nil, nil
	default:
		return "", "", nil, errors.New("tool is not forwardable")
	}
}

// searchQuery converts kenwea.marketplace.search arguments into the query string
// GET /products expects, and returns "" when nothing usable was supplied.
//
// Only the seven parameters the platform handler actually reads are forwarded. An
// unrecognised argument is dropped rather than passed through, so a caller cannot
// smuggle a filter the schema does not declare.
//
// Numbers arrive as JSON numbers, so they are decoded as such and re-rendered; taking
// them as strings would have made the schema lie about their type.
func searchQuery(params json.RawMessage) string {
	if len(params) == 0 || string(params) == "null" {
		return ""
	}
	var body struct {
		Q             string `json:"q"`
		Category      string `json:"category"`
		MinPriceCents *int64 `json:"minPriceCents"`
		MaxPriceCents *int64 `json:"maxPriceCents"`
		Sort          string `json:"sort"`
		Limit         *int   `json:"limit"`
		Offset        *int   `json:"offset"`
	}
	if err := json.Unmarshal(params, &body); err != nil {
		return ""
	}
	values := url.Values{}
	for key, value := range map[string]string{
		"q":        strings.TrimSpace(body.Q),
		"category": strings.TrimSpace(body.Category),
		"sort":     strings.TrimSpace(body.Sort),
	} {
		if value != "" {
			values.Set(key, value)
		}
	}
	if body.MinPriceCents != nil {
		values.Set("minPriceCents", strconv.FormatInt(*body.MinPriceCents, 10))
	}
	if body.MaxPriceCents != nil {
		values.Set("maxPriceCents", strconv.FormatInt(*body.MaxPriceCents, 10))
	}
	if body.Limit != nil {
		values.Set("limit", strconv.Itoa(*body.Limit))
	}
	if body.Offset != nil {
		values.Set("offset", strconv.Itoa(*body.Offset))
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}

func paramValue(params json.RawMessage, key string) string {
	var body map[string]string
	_ = json.Unmarshal(params, &body)
	return body[key]
}

// setForwardedClient tells the platform which network address the caller came
// from, so its per-address rate limits count callers rather than this server.
//
// Until 2026-09-29 the platform saw every MCP call as coming from this
// container: production had 29 actor buckets for sandbox checks and 3 address
// buckets. nginx sets X-Real-IP to the connecting address and overwrites any
// value a client sends, and this server listens only on localhost, so the header
// is the caller's address. The platform believes it only alongside the internal
// forwarding token, which a caller reaching the API directly does not have.
func setForwardedClient(req *http.Request, incoming *http.Request) {
	token := strings.TrimSpace(os.Getenv("KENWEA_INTERNAL_FORWARD_TOKEN"))
	if token == "" {
		return
	}
	ip := strings.TrimSpace(incoming.Header.Get("X-Real-IP"))
	if net.ParseIP(ip) == nil {
		host, _, err := net.SplitHostPort(incoming.RemoteAddr)
		if err != nil {
			return
		}
		ip = host
	}
	req.Header.Set("X-Kenwea-Client-IP", ip)
	req.Header.Set("X-Kenwea-Forward-Token", token)
}
