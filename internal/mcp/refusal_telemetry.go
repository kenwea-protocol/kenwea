package mcp

import (
	"encoding/json"
	"log"
	"net/http"
)

// Refusal telemetry: one log line per request this server refuses with an HTTP
// 4xx, naming the reason code from the response and the caller's user agent.
//
// Found 2026-09-25: over a week, 117 POSTs to /mcp/v1 were answered 400 and the
// logs could not say why, because the reason lived only in the response body.
// Working it out took sending candidate malformed requests and matching byte
// counts in the access log against a collector's. The line below makes that a
// grep.
//
// What it deliberately does not record, for the same reasons as the rest of this
// package's telemetry: no request body, no params, no key, no actor id, and no
// IP. The user agent is the self-description a client chose to send; it is
// sanitised so a crafted value cannot forge a second log line.
type refusalRecorder struct {
	http.ResponseWriter
	status int
	logged bool
	ua     string
}

func (r *refusalRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *refusalRecorder) Write(b []byte) (int, error) {
	if !r.logged && r.status >= 400 && r.status < 500 {
		r.logged = true
		log.Printf("mcp.refusal.telemetry status=%d reason=%q ua=%q", r.status, refusalReason(b), r.ua)
	}
	return r.ResponseWriter.Write(b)
}

// refusalReason reads the reason code out of a JSON-RPC error body. This
// server's own errors carry a code-like string in message (invalid_json,
// unauthorized, ...) or the spec's wording (Header mismatch, Unsupported protocol
// version), so message plus the numeric code is enough to tell them apart.
func refusalReason(body []byte) string {
	var envelope struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Detail string `json:"detail"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Error == nil {
		return "(no json-rpc error)"
	}
	reason := envelope.Error.Message
	if envelope.Error.Code == codeHeaderMismatch && envelope.Error.Data.Detail != "" {
		// For a header mismatch the useful part is which header.
		reason += ": " + envelope.Error.Data.Detail
	}
	return sanitizeTelemetryValue(reason)
}
