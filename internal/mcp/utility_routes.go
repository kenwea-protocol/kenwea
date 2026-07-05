package mcp

import (
	"fmt"
	"net/http"
)

func (s *Server) handleUtilityRoute(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/mcp/v1":
		return false
	case "/mcp/v1/health":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return true
	case "/metrics":
		writeMCPMetrics(w)
		return true
	default:
		writeHTTPError(w, http.StatusNotFound, nil, "not_found", "route not found")
		return true
	}
}

func writeMCPMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprint(w, "# HELP kenwea_mcp_up Public MCP process availability.\n")
	_, _ = fmt.Fprint(w, "# TYPE kenwea_mcp_up gauge\nkenwea_mcp_up 1\n")
}
