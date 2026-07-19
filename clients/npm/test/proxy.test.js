import { test } from "node:test";
import assert from "node:assert/strict";
import { Readable, Writable } from "node:stream";

import { forwardOne, runProxy } from "../src/proxy.js";
import { resolveConfig } from "../src/config.js";

/**
 * Build a fake fetch that records the requests it receives and replays canned
 * responses in order.
 * @param {Array<{status?: number, headers?: Record<string,string>, body?: any}>} responses
 */
function fakeFetch(responses) {
  /** @type {Array<{url: any, init: any}>} */
  const calls = [];
  let i = 0;
  /** @param {any} url @param {any} init */
  const impl = async (url, init) => {
    calls.push({ url, init });
    const spec = responses[Math.min(i, responses.length - 1)];
    i++;
    const status = spec.status ?? 200;
    const headers = new Headers(spec.headers ?? {});
    const body = spec.body === undefined ? "" : JSON.stringify(spec.body);
    return new Response(body, { status, headers });
  };
  return { impl, calls };
}

/** Collect everything written to a stream as a single string. */
function collector() {
  /** @type {string[]} */
  const chunks = [];
  const stream = new Writable({
    write(chunk, _enc, cb) {
      chunks.push(chunk.toString());
      cb();
    },
  });
  return { stream, text: () => chunks.join("") };
}

test("forwardOne: returns parsed body and captures the issued session id", async () => {
  const config = resolveConfig({}, { apiKey: "k" });
  const { impl, calls } = fakeFetch([
    {
      headers: { "Mcp-Session-Id": "sess-abc" },
      body: { jsonrpc: "2.0", id: 1, result: { products: [] } },
    },
  ]);
  const session = { sessionId: null };
  const res = await forwardOne(
    { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "kenwea.marketplace.search", arguments: {} } },
    config,
    session,
    impl,
    () => "gen-id",
  );
  assert.deepEqual(res, { jsonrpc: "2.0", id: 1, result: { products: [] } });
  assert.equal(session.sessionId, "sess-abc");
  assert.equal(calls[0].init.headers["Authorization"], "Bearer k");
  // A read tool must NOT get an idempotency key.
  assert.equal(calls[0].init.headers["Idempotency-Key"], undefined);
});

test("forwardOne: mutating tool gets a generated Idempotency-Key", async () => {
  const config = resolveConfig({}, { apiKey: "k" });
  const { impl, calls } = fakeFetch([{ body: { jsonrpc: "2.0", id: 2, result: {} } }]);
  await forwardOne(
    { jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "kenwea.marketplace.purchase", arguments: {} } },
    config,
    { sessionId: null },
    impl,
    () => "generated-idem-key",
  );
  assert.equal(calls[0].init.headers["Idempotency-Key"], "generated-idem-key");
});

test("forwardOne: mutating tool honours a caller-pinned idempotency key", async () => {
  const config = resolveConfig({}, { apiKey: "k" });
  const { impl, calls } = fakeFetch([{ body: { jsonrpc: "2.0", id: 3, result: {} } }]);
  await forwardOne(
    {
      jsonrpc: "2.0",
      id: 3,
      method: "tools/call",
      params: { name: "kenwea.orders.submitBid", arguments: { _meta: { idempotencyKey: "pinned-1" } } },
    },
    config,
    { sessionId: null },
    impl,
    () => "should-not-be-used",
  );
  assert.equal(calls[0].init.headers["Idempotency-Key"], "pinned-1");
});

test("forwardOne: notification returns null (no response emitted)", async () => {
  const config = resolveConfig({});
  const { impl } = fakeFetch([{ status: 202 }]);
  const res = await forwardOne(
    { jsonrpc: "2.0", method: "notifications/initialized" },
    config,
    { sessionId: null },
    impl,
    () => "x",
  );
  assert.equal(res, null);
});

test("forwardOne: empty body for a request yields a structured error", async () => {
  const config = resolveConfig({});
  const { impl } = fakeFetch([{ status: 200, body: undefined }]);
  const res = /** @type {any} */ (
    await forwardOne(
      { jsonrpc: "2.0", id: 7, method: "tools/list", params: {} },
      config,
      { sessionId: null },
      impl,
      () => "x",
    )
  );
  assert.equal(res.id, 7);
  assert.equal(res.error.message, "empty_upstream_response");
});

test("runProxy: end-to-end over stdio framing", async () => {
  const config = resolveConfig({}, { apiKey: "k" });
  const { impl } = fakeFetch([
    { headers: { "Mcp-Session-Id": "s1" }, body: { jsonrpc: "2.0", id: 1, result: { serverInfo: { name: "kenwea-public-mcp" } } } },
    { status: 202 }, // notifications/initialized
    { body: { jsonrpc: "2.0", id: 2, result: { tools: [] } } },
  ]);
  const input =
    JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: {} }) +
    "\n" +
    JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }) +
    "\n" +
    JSON.stringify({ jsonrpc: "2.0", id: 2, method: "tools/list", params: {} }) +
    "\n";

  const out = collector();
  const err = collector();
  await runProxy(config, {
    stdin: Readable.from([input]),
    stdout: out.stream,
    stderr: err.stream,
    fetchImpl: impl,
    generateId: () => "x",
  });

  const lines = out.text().trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
  // Two responses (initialize, tools/list); the notification produced none.
  assert.equal(lines.length, 2);
  assert.equal(lines[0].id, 1);
  assert.equal(lines[1].id, 2);
});

test("runProxy: unparseable line yields a JSON-RPC parse error", async () => {
  const config = resolveConfig({});
  const { impl } = fakeFetch([{ body: {} }]);
  const out = collector();
  await runProxy(config, {
    stdin: Readable.from(["not json\n"]),
    stdout: out.stream,
    stderr: collector().stream,
    fetchImpl: impl,
    generateId: () => "x",
  });
  const line = JSON.parse(out.text().trim());
  assert.equal(line.error.code, -32700);
  assert.equal(line.error.message, "parse_error");
});

test("runProxy: transport failure on a request surfaces as a bridge error", async () => {
  const config = resolveConfig({}, { apiKey: "leak-me" });
  const failing = async () => {
    throw new Error("connect ECONNREFUSED leak-me");
  };
  const out = collector();
  await runProxy(config, {
    stdin: Readable.from([JSON.stringify({ jsonrpc: "2.0", id: 5, method: "tools/list", params: {} }) + "\n"]),
    stdout: out.stream,
    stderr: collector().stream,
    fetchImpl: failing,
    generateId: () => "x",
  });
  const line = JSON.parse(out.text().trim());
  assert.equal(line.id, 5);
  assert.equal(line.error.message, "bridge_transport_error");
  // The api key must be redacted from the error detail.
  assert.ok(!line.error.data.detail.includes("leak-me"));
  assert.ok(line.error.data.detail.includes("***"));
});
