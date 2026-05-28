package platformapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp"
)

type Authenticator struct {
	BaseURL string
	Client  *http.Client
}

func New(baseURL string) Authenticator {
	return Authenticator{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (a Authenticator) Authenticate(r *http.Request) (mcp.AuthResult, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.BaseURL+"/internal/mcp/identify", nil)
	if err != nil {
		return mcp.AuthResult{}, err
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
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

func (a Authenticator) ForwardTool(r *http.Request, method string, params json.RawMessage) (map[string]any, error) {
	httpMethod, path, body, err := route(method, params)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(r.Context(), httpMethod, a.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client().Do(req)
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
		Error any            `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("platform api rejected tool %s", method)
	}
	return envelope.Data, nil
}

func route(method string, params json.RawMessage) (string, string, io.Reader, error) {
	switch method {
	case "kenwea.onboarding.registerSelf":
		return http.MethodPost, "/agent/self-registration", bytes.NewReader(params), nil
	case "kenwea.marketplace.search":
		return http.MethodGet, "/products", nil, nil
	case "kenwea.marketplace.preview":
		return http.MethodPost, "/agent/products/preview", bytes.NewReader(params), nil
	case "kenwea.marketplace.publish":
		return http.MethodPost, "/agent/products/publish", bytes.NewReader(params), nil
	case "kenwea.marketplace.purchase":
		return http.MethodPost, "/agent/purchases", bytes.NewReader(params), nil
	case "kenwea.marketplace.install":
		return http.MethodPost, "/agent/installations", bytes.NewReader(params), nil
	case "kenwea.wallet.balance":
		return http.MethodGet, "/agent/wallet", nil, nil
	case "kenwea.wallet.transactions":
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
	case "kenwea.procurement.memory":
		return http.MethodGet, "/agent/procurement", nil, nil
	case "kenwea.reputation.graph":
		id := paramValue(params, "agentId")
		if id == "" {
			return "", "", nil, errors.New("agentId is required")
		}
		return http.MethodGet, "/agents/" + url.PathEscape(id) + "/reputation", nil, nil
	case "kenwea.community.ask":
		return http.MethodPost, "/assistant/questions", bytes.NewReader(params), nil
	case "kenwea.observer.feed":
		cursor := paramValue(params, "cursor")
		if cursor != "" {
			return http.MethodGet, "/observer/feed?cursor=" + url.QueryEscape(cursor), nil, nil
		}
		return http.MethodGet, "/observer/feed", nil, nil
	case "kenwea.analytics.forecast":
		return http.MethodGet, "/analytics/forecast", nil, nil
	case "kenwea.recommendations.relatedProducts":
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
	case "kenwea.scale.status":
		return http.MethodGet, "/scale/status", nil, nil
	default:
		return "", "", nil, errors.New("tool is not forwardable")
	}
}

func paramValue(params json.RawMessage, key string) string {
	var body map[string]string
	_ = json.Unmarshal(params, &body)
	return body[key]
}
