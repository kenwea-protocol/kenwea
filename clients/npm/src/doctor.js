// Kenwea MCP bridge — `kenwea doctor` connectivity + auth check.
//
// Runs a short, read-only sequence against the remote endpoint and prints a
// PASS/FAIL line per step. Never prints the api key. Returns ok=false if any
// required step failed so the CLI can exit non-zero.

import { buildHeaders } from "./protocol.js";
import { redact } from "./config.js";

/**
 * @typedef {import("./config.js").BridgeConfig} BridgeConfig
 */

/**
 * @param {string} url endpoint like https://mcp.kenwea.com/mcp/v1
 * @returns {string}
 */
export function deriveHealthUrl(url) {
  return url.replace(/\/+$/, "") + "/health";
}

/**
 * @param {BridgeConfig} config
 * @param {{fetchImpl?: typeof fetch, stdout?: NodeJS.WritableStream}} [io]
 * @returns {Promise<{ok: boolean, steps: Array<{name: string, ok: boolean, detail: string}>}>}
 */
export async function runDoctor(config, io = {}) {
  const doFetch = io.fetchImpl ?? fetch;
  const out = io.stdout ?? process.stdout;

  /** @type {Array<{name: string, ok: boolean, detail: string}>} */
  const steps = [];
  /** @type {{sessionId: string|null}} */
  const session = { sessionId: null };

  // 1. Health probe (unauthenticated).
  try {
    const res = await doFetch(deriveHealthUrl(config.url), { method: "GET" });
    steps.push({
      name: "health",
      ok: res.ok,
      detail: res.ok ? `HTTP ${res.status}` : `HTTP ${res.status}`,
    });
  } catch (err) {
    steps.push({ name: "health", ok: false, detail: errText(err, config) });
  }

  // 2. initialize handshake.
  const initRes = await rpc(doFetch, config, session, {
    jsonrpc: "2.0",
    id: "doctor-initialize",
    method: "initialize",
    params: {
      protocolVersion: config.protocolVersion,
      capabilities: {},
      clientInfo: { name: "kenwea-mcp-doctor", version: "0.1.0" },
    },
  });
  steps.push(rpcStep("initialize", initRes));

  // 3. tools/list.
  const listRes = await rpc(doFetch, config, session, {
    jsonrpc: "2.0",
    id: "doctor-tools-list",
    method: "tools/list",
    params: {},
  });
  const toolCount =
    listRes.body &&
    listRes.body.result &&
    Array.isArray(listRes.body.result.tools)
      ? listRes.body.result.tools.length
      : 0;
  steps.push({
    name: "tools/list",
    ok: listRes.ok && toolCount > 0,
    detail: listRes.ok ? `${toolCount} tools` : rpcDetail(listRes),
  });

  // 4. Authenticated read (only meaningful with a key).
  if (config.apiKey) {
    const searchRes = await rpc(doFetch, config, session, {
      jsonrpc: "2.0",
      id: "doctor-search",
      method: "tools/call",
      params: { name: "kenwea.marketplace.search", arguments: { query: "" } },
    });
    steps.push({
      name: "auth (marketplace.search)",
      ok: searchRes.ok,
      detail: searchRes.ok ? "authenticated read OK" : rpcDetail(searchRes),
    });
  } else {
    steps.push({
      name: "auth",
      ok: true,
      detail: "skipped — no KENWEA_API_KEY (anonymous session; only initialize/tools/list/registerSelf work without one)",
    });
  }

  const ok = steps.every((s) => s.ok);
  out.write(`Kenwea MCP doctor — ${config.url}\n`);
  for (const step of steps) {
    out.write(`  ${step.ok ? "PASS" : "FAIL"}  ${step.name}: ${step.detail}\n`);
  }
  out.write(ok ? "\nAll checks passed.\n" : "\nOne or more checks failed.\n");
  return { ok, steps };
}

/**
 * @param {typeof fetch} doFetch
 * @param {BridgeConfig} config
 * @param {{sessionId: string|null}} session
 * @param {object} message
 * @returns {Promise<{ok: boolean, status: number, body: any, error: string|null}>}
 */
async function rpc(doFetch, config, session, message) {
  try {
    const res = await doFetch(config.url, {
      method: "POST",
      headers: buildHeaders(config, session, null),
      body: JSON.stringify(message),
    });
    const issued = res.headers.get("Mcp-Session-Id");
    if (issued) session.sessionId = issued;
    const text = await res.text();
    /** @type {any} */
    let body = null;
    try {
      body = text ? JSON.parse(text) : null;
    } catch {
      body = null;
    }
    const ok = res.ok && !!body && body.error === undefined;
    return { ok, status: res.status, body, error: null };
  } catch (err) {
    return { ok: false, status: 0, body: null, error: errText(err, config) };
  }
}

/**
 * @param {string} name
 * @param {{ok: boolean, status: number, body: any, error: string|null}} res
 * @returns {{name: string, ok: boolean, detail: string}}
 */
function rpcStep(name, res) {
  return { name, ok: res.ok, detail: res.ok ? `HTTP ${res.status}` : rpcDetail(res) };
}

/**
 * @param {{status: number, body: any, error: string|null}} res
 * @returns {string}
 */
function rpcDetail(res) {
  if (res.error) return res.error;
  if (res.body && res.body.error) {
    const e = res.body.error;
    const detail = e.data && e.data.detail ? `: ${e.data.detail}` : "";
    return `${e.message || "error"}${detail} (HTTP ${res.status})`;
  }
  return `HTTP ${res.status}`;
}

/**
 * @param {unknown} err
 * @param {BridgeConfig} config
 * @returns {string}
 */
function errText(err, config) {
  return redact(String((err && /** @type {any} */ (err).message) || err), config);
}
