import { test } from "node:test";
import assert from "node:assert/strict";

import {
  buildHeaders,
  messageProtocolVersion,
  encodeHeaderValue,
  routingHeaders,
} from "../src/protocol.js";
import { resolveConfig, STATELESS_PROTOCOL_VERSION } from "../src/config.js";

// A local client speaking the 2026-07-28 revision puts its version in params._meta.
// The bridge must forward such a request with that version in the header, the
// routing headers the revision requires, and no session id. Every other message must
// leave exactly as it did before.

const modernMeta = {
  "io.modelcontextprotocol/protocolVersion": STATELESS_PROTOCOL_VERSION,
  "io.modelcontextprotocol/clientCapabilities": {},
};

test("messageProtocolVersion reads only params._meta", () => {
  assert.equal(messageProtocolVersion({ method: "tools/list", params: { _meta: modernMeta } }), "2026-07-28");
  assert.equal(messageProtocolVersion({ method: "tools/list", params: {} }), null);
  assert.equal(messageProtocolVersion({ method: "initialize", params: { protocolVersion: "2025-11-25" } }), null);
  assert.equal(messageProtocolVersion(null), null);
});

test("a stateless tools/call carries its version, both routing headers and no session", () => {
  const config = resolveConfig({}, { apiKey: "secret-key" });
  const msg = {
    jsonrpc: "2.0",
    id: 1,
    method: "tools/call",
    params: { name: "kenwea.sandbox.check", arguments: {}, _meta: modernMeta },
  };
  const headers = buildHeaders(config, { sessionId: "sess-legacy" }, null, msg);
  assert.equal(headers["MCP-Protocol-Version"], "2026-07-28");
  assert.equal(headers["Mcp-Method"], "tools/call");
  assert.equal(headers["Mcp-Name"], "kenwea.sandbox.check");
  assert.equal(headers["Mcp-Session-Id"], undefined);
  assert.equal(headers["Authorization"], "Bearer secret-key");
});

test("a stateless tools/list gets Mcp-Method and no Mcp-Name", () => {
  const headers = buildHeaders(resolveConfig({}), { sessionId: null }, null, {
    jsonrpc: "2.0",
    id: 2,
    method: "tools/list",
    params: { _meta: modernMeta },
  });
  assert.equal(headers["Mcp-Method"], "tools/list");
  assert.equal(headers["Mcp-Name"], undefined);
});

test("a legacy message is forwarded exactly as before", () => {
  const config = resolveConfig({}, { apiKey: "secret-key" });
  const msg = { jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "kenwea.sandbox.check", arguments: {} } };
  const withMsg = buildHeaders(config, { sessionId: "sess-9" }, null, msg);
  const without = buildHeaders(config, { sessionId: "sess-9" }, null);
  assert.deepEqual(withMsg, without);
  assert.equal(withMsg["MCP-Protocol-Version"], "2025-11-25");
  assert.equal(withMsg["Mcp-Session-Id"], "sess-9");
  assert.equal(withMsg["Mcp-Method"], undefined);
});

test("header values that are not header-safe are base64 encoded", () => {
  assert.equal(encodeHeaderValue("kenwea.sandbox.check"), "kenwea.sandbox.check");
  assert.equal(encodeHeaderValue("file:///tmp/a b"), "file:///tmp/a b");
  assert.equal(encodeHeaderValue(" padded"), `=?base64?${Buffer.from(" padded").toString("base64")}?=`);
  assert.equal(encodeHeaderValue("ürün"), `=?base64?${Buffer.from("ürün").toString("base64")}?=`);
  assert.deepEqual(routingHeaders({ method: "resources/read", params: { uri: "kenwea://x" } }), {
    "Mcp-Method": "resources/read",
    "Mcp-Name": "kenwea://x",
  });
});
