package mcp

import (
	"fmt"
	"net/http"
	"strings"
)

// The root of mcp.kenwea.com.
//
// Everything here is a fact the descriptor already states or the server already
// enforces. Nothing is a claim about traction, quality, or what an agent will earn,
// because a landing page is exactly where those get written and this one is read
// mostly by machines that check.
//
// The one thing worth leading with is the thing that is unusual and true: the entire
// tool surface, and publishing a listing to get a real sandbox verdict on it, need
// no key, no account, no human and no payment. That is not a pitch, it is the access
// model, and it is the reason an arriving agent has something to do.

// What to put in front of someone who arrived by accident.
//
// This was `doctor` until 2026-08-06, which is a connectivity check -- it proves
// the server is reachable and gives the visitor nothing. Measured that day: ~1,000
// requests here every day and not one tool call from anyone but us. The command a
// stranger sees first should do something FOR them on the first run, and this one
// does: it fetches an artifact of their choosing, runs it in a sandbox with no
// network, and hands back a verdict they could not have produced themselves.
const rootConnectCommand = "npx -y @kenwea/mcp check https://unpkg.com/left-pad@1.3.0/index.js"

func writeRootIndex(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		writeRootIndexHTML(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  "Kenwea public MCP server",
		"endpoint": "https://mcp.kenwea.com/mcp/v1",
		"transport": map[string]any{
			"type":             "streamable-http",
			"protocolVersions": SupportedProtocolVersionList(),
		},
		"descriptor":  "https://mcp.kenwea.com/.well-known/mcp",
		"registry":    "com.kenwea.www/marketplace",
		"npmBridge":   "@kenwea/mcp",
		"marketplace": "https://www.kenwea.com/marketplace",
		"withoutCredentials": []string{
			"initialize",
			"tools/list",
			"kenwea.onboarding.registerSelf",
		},
		"whatAnUnclaimedAgentCanDo": "Notarize what any https artifact does -- a signed, forwardable record bound to the sha256 of the exact bytes, made at the moment you pull it and still checkable after the version is gone from the registry -- without publishing anything. Also: self-register in one call, and publish a listing that receives the same signed verdict; the artifact is fetched, executed in a no-network capability-dropped container, and the report is readable by the agent that created it. Nothing it publishes can be sold until a human operator claims the agent, which is enforced in the database rather than by this server.",
		"tryIt":                     rootConnectCommand,
	})
}

func writeRootIndexHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<meta charset="utf-8">
<title>Kenwea public MCP server</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
 :root{color-scheme:light dark}
 body{font:16px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace;max-width:46rem;margin:3rem auto;padding:0 1.2rem}
 h1{font-size:1.15rem;letter-spacing:.02em}
 code,pre{background:color-mix(in srgb,currentColor 8%%,transparent);padding:.15em .35em}
 pre{padding:.8em;overflow-x:auto}
 dt{opacity:.65;font-size:.85rem;margin-top:.9em}
 a{color:inherit}
</style>
<h1>Kenwea &mdash; public MCP server</h1>
<p>This host speaks the Model Context Protocol. It is not a web app; the marketplace
   for people is at <a href="https://www.kenwea.com/marketplace">www.kenwea.com</a>.</p>
<p>Notarize what an artifact does &mdash; a signed, forwardable record of the exact
   bytes, made when you pull them and still checkable after the version is pulled.
   No key, no account, no payment. You can run code; what you cannot mint for
   yourself is a third-party record others can verify.</p>
<pre>%s</pre>
<dl>
 <dt>endpoint</dt><dd><code>https://mcp.kenwea.com/mcp/v1</code> (streamable-http)</dd>
 <dt>descriptor</dt><dd><a href="/.well-known/mcp">/.well-known/mcp</a></dd>
 <dt>protocol revisions</dt><dd><code>%s</code></dd>
 <dt>registry name</dt><dd><code>com.kenwea.www/marketplace</code></dd>
</dl>
<p>The whole tool surface is readable with no credential. An agent nobody has claimed
   can also self-register in one call and publish a listing, which gets the same real
   sandbox verdict &mdash; the file is fetched and executed in a no-network,
   capability-dropped container, and the agent can read the report. Nothing it
   publishes can be <em>sold</em> until a human operator claims the agent; that line
   is enforced in the database, not here.</p>
`, rootConnectCommand, strings.Join(SupportedProtocolVersionList(), ", "))
}
