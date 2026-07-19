import { test } from "node:test";
import assert from "node:assert/strict";

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
