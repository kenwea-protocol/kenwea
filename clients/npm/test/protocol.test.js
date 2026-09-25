import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

import {
  isNotification,
  toolNameOf,
  needsIdempotency,
  explicitIdempotencyKey,
  buildHeaders,
  IDEMPOTENT_TOOLS,
} from "../src/protocol.js";
import { resolveConfig, validateConfig, redact } from "../src/config.js";

test("isNotification: method without id is a notification", () => {
  assert.equal(isNotification({ jsonrpc: "2.0", method: "notifications/initialized" }), true);
  assert.equal(isNotification({ jsonrpc: "2.0", id: 1, method: "tools/list" }), false);
  assert.equal(isNotification({ jsonrpc: "2.0", id: 1, result: {} }), false);
  assert.equal(isNotification(null), false);
  assert.equal(isNotification([{ method: "x" }]), false);
});

test("toolNameOf: resolves tools/call name and direct methods", () => {
  assert.equal(
    toolNameOf({ method: "tools/call", params: { name: "kenwea.marketplace.publish" } }),
    "kenwea.marketplace.publish",
  );
  assert.equal(toolNameOf({ method: "kenwea.marketplace.search" }), "kenwea.marketplace.search");
  assert.equal(toolNameOf({ method: "tools/call", params: {} }), null);
  assert.equal(toolNameOf({ id: 1 }), null);
});

test("needsIdempotency: only mutating tools require a key", () => {
  assert.equal(needsIdempotency({ method: "kenwea.marketplace.publish" }), true);
  assert.equal(
    needsIdempotency({ method: "tools/call", params: { name: "kenwea.orders.submitBid" } }),
    true,
  );
  assert.equal(needsIdempotency({ method: "kenwea.marketplace.search" }), false);
  assert.equal(needsIdempotency({ method: "tools/list" }), false);
});

test("IDEMPOTENT_TOOLS matches the server's idempotent tool set", () => {
  // Mirrors idempotentTools in apps/mcp-server/internal/mcp/tools.go.
  const expected = [
    "kenwea.marketplace.publish",
    "kenwea.marketplace.purchase",
    "kenwea.marketplace.install",
    "kenwea.notifications.ack",
    "kenwea.orders.submitBid",
    "kenwea.orders.deliver",
    "kenwea.collab.create",
    "kenwea.collab.join",
    "kenwea.dependencies.watch",
    "kenwea.onboarding.startOperatorAgent",
  ];
  assert.deepEqual([...IDEMPOTENT_TOOLS].sort(), expected.slice().sort());
});

test("explicitIdempotencyKey: honours _meta on params and arguments", () => {
  assert.equal(
    explicitIdempotencyKey({ method: "x", params: { _meta: { idempotencyKey: "abc" } } }),
    "abc",
  );
  assert.equal(
    explicitIdempotencyKey({
      method: "tools/call",
      params: { name: "t", arguments: { _meta: { idempotencyKey: "def" } } },
    }),
    "def",
  );
  assert.equal(explicitIdempotencyKey({ method: "x", params: {} }), null);
  assert.equal(explicitIdempotencyKey({ method: "x" }), null);
  assert.equal(
    explicitIdempotencyKey({ method: "x", params: { _meta: { idempotencyKey: "  " } } }),
    null,
  );
});

test("buildHeaders: attaches auth, session, protocol, idempotency", () => {
  const config = resolveConfig({}, { apiKey: "secret-key", correlationId: "trace-1" });
  const headers = buildHeaders(config, { sessionId: "sess-9" }, "idem-1");
  assert.equal(headers["Authorization"], "Bearer secret-key");
  assert.equal(headers["Mcp-Session-Id"], "sess-9");
  assert.equal(headers["Idempotency-Key"], "idem-1");
  assert.equal(headers["MCP-Protocol-Version"], "2025-11-25");
  assert.equal(headers["X-Correlation-ID"], "trace-1");
  assert.equal(headers["content-type"], "application/json");
});

test("buildHeaders: tourist mode omits Authorization", () => {
  const config = resolveConfig({});
  const headers = buildHeaders(config, { sessionId: null });
  assert.equal(headers["Authorization"], undefined);
  assert.equal(headers["Mcp-Session-Id"], undefined);
});

test("resolveConfig: overrides beat env beat defaults", () => {
  const config = resolveConfig(
    { KENWEA_MCP_URL: "https://env.example/mcp/v1", KENWEA_API_KEY: "env-key" },
    { apiKey: "override-key" },
  );
  assert.equal(config.url, "https://env.example/mcp/v1");
  assert.equal(config.apiKey, "override-key");
  assert.equal(config.protocolVersion, "2025-11-25");
});

test("validateConfig: rejects bad url and protocol", () => {
  assert.equal(validateConfig(resolveConfig({}, { url: "https://mcp.kenwea.com/mcp/v1" })), null);
  assert.match(String(validateConfig(resolveConfig({}, { url: "ftp://x" }))), /must be http/);
  assert.match(
    String(validateConfig(resolveConfig({}, { protocolVersion: "1999-01-01" }))),
    /unsupported MCP-Protocol-Version/,
  );
});

test("redact: removes the api key from arbitrary text", () => {
  const config = resolveConfig({}, { apiKey: "top-secret" });
  assert.equal(redact("failed with Bearer top-secret at host", config), "failed with Bearer *** at host");
  const tourist = resolveConfig({});
  assert.equal(redact("no key here", tourist), "no key here");
});

// --- version and protocol-set drift -----------------------------------------

test("--version reports the published package version", async () => {
  // Two releases shipped as 0.1.2 while the CLI answered 0.1.0, because the value
  // was restated in cli.js under a comment claiming it was kept in sync. Nothing
  // compared them, so nothing noticed. This is that comparison.
  const pkg = JSON.parse(
    readFileSync(new URL("../package.json", import.meta.url), "utf8"),
  );
  const cli = readFileSync(new URL("../src/cli.js", import.meta.url), "utf8");
  assert.ok(
    !/const VERSION = "\d/.test(cli),
    "cli.js hardcodes a version string; read it from package.json instead",
  );
  const { stdout } = await execFileAsync(process.execPath, [
    fileURLToPath(new URL("../src/cli.js", import.meta.url)),
    "--version",
  ]);
  assert.equal(stdout.trim(), pkg.version);
});

test("the accepted protocol set matches what the live server publishes", async (t) => {
  // The bridge and the server each carry a list of revisions. When the server was
  // widened to accept 2025-06-18, the Python client was not, and a client
  // configured for that revision was refused locally before it reached the
  // network. This is a network test by necessity: the whole point is that the two
  // sides can disagree, and only one of them is in this repo.
  const response = await fetch("https://mcp.kenwea.com/.well-known/mcp");
  if (!response.ok) {
    t.skip(`descriptor unreachable (HTTP ${response.status})`);
    return;
  }
  /** @type {any} */
  const descriptor = await response.json();
  /** @type {string[]} */
  const served = descriptor.transport?.protocolVersions ?? [];
  const config = readFileSync(new URL("../src/config.js", import.meta.url), "utf8");
  for (const version of served) {
    assert.ok(
      config.includes(version),
      `the server accepts ${version} but the bridge does not list it`,
    );
  }
});
