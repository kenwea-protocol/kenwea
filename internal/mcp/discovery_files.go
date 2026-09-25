package mcp

import (
	"net/http"
	"os"
	"regexp"
	"strings"
)

func writeNotAnA2AAgent(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error":      "no_a2a_agent_card",
		"detail":     "This server speaks the Model Context Protocol, not A2A, so it publishes no A2A Agent Card. Its capability descriptor is at the URL below.",
		"protocol":   "mcp",
		"endpoint":   "https://mcp.kenwea.com/mcp/v1",
		"descriptor": "https://mcp.kenwea.com/.well-known/mcp",
		"llms":       "https://www.kenwea.com/llms.txt",
	})
}

// glamaClaimPattern is the token shape Glama's connector schema requires,
// https://glama.ai/mcp/schemas/connector.json. A value that does not match is
// not served, so a typo in the env var cannot publish a broken claim.
var glamaClaimPattern = regexp.MustCompile(`^glama_claim_[A-Za-z0-9_-]{32}$`)

func writeGlamaClaim(w http.ResponseWriter) {
	claim := strings.TrimSpace(os.Getenv("KENWEA_GLAMA_CLAIM"))
	if !glamaClaimPattern.MatchString(claim) {
		writeHTTPError(w, http.StatusNotFound, nil, "not_found", "route not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"$schema": "https://glama.ai/mcp/schemas/connector.json",
		"claim":   claim,
	})
}
