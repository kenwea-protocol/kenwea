// Kenwea MCP bridge — pure JSON-RPC / MCP protocol helpers.
//
// These functions carry the entire policy surface the bridge needs to classify a
// message before forwarding it. They are pure so the behaviour that decides
// "does this need an Idempotency-Key" is unit-tested in isolation.

/**
 * @typedef {import("./config.js").BridgeConfig} BridgeConfig
 */

/**
 * Tools the Kenwea public MCP server requires an Idempotency-Key for. Mirrors
 * `idempotentTools` in apps/mcp-server/internal/mcp/tools.go. Kept in sync via
 * the conformance suite (packages/contracts), which fails if the surfaces drift.
 * @type {Set<string>}
 */
export const IDEMPOTENT_TOOLS = new Set([
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
]);

/**
 * A JSON-RPC notification is a message with a method but no id. The server
 * answers these with 202 and no body, so the bridge must NOT emit a response.
 * @param {any} msg
 * @returns {boolean}
 */
export function isNotification(msg) {
  return (
    !!msg &&
    typeof msg === "object" &&
    !Array.isArray(msg) &&
    typeof msg.method === "string" &&
    msg.id === undefined
  );
}

/**
 * The effective Kenwea tool a message targets. For a standard MCP `tools/call`
 * this is params.name; for a direct `kenwea.*` JSON-RPC call it is the method.
 * @param {any} msg
 * @returns {string|null}
 */
export function toolNameOf(msg) {
  if (!msg || typeof msg !== "object" || typeof msg.method !== "string") {
    return null;
  }
  if (msg.method === "tools/call") {
    const name =
      msg.params && typeof msg.params === "object" ? msg.params.name : undefined;
    return typeof name === "string" ? name : null;
  }
  return msg.method;
}

/**
 * Whether a message targets a tool that requires an Idempotency-Key.
 * @param {any} msg
 * @returns {boolean}
 */
export function needsIdempotency(msg) {
  const name = toolNameOf(msg);
  return name != null && IDEMPOTENT_TOOLS.has(name);
}

/**
 * A caller may pin an idempotency key via `params._meta.idempotencyKey` (direct
 * call) or `params.arguments._meta.idempotencyKey` (tools/call). If present it is
 * honoured so a client can make a retry safely idempotent; otherwise the bridge
 * generates a fresh one per mutating request.
 * @param {any} msg
 * @returns {string|null}
 */
export function explicitIdempotencyKey(msg) {
  const params = msg && typeof msg === "object" ? msg.params : undefined;
  if (!params || typeof params !== "object") return null;
  const direct = readMetaKey(params);
  if (direct) return direct;
  const args = params.arguments;
  if (args && typeof args === "object") {
    const nested = readMetaKey(args);
    if (nested) return nested;
  }
  return null;
}

/**
 * @param {any} container
 * @returns {string|null}
 */
function readMetaKey(container) {
  const meta = container._meta;
  if (meta && typeof meta === "object" && typeof meta.idempotencyKey === "string") {
    const value = meta.idempotencyKey.trim();
    return value || null;
  }
  return null;
}

/**
 * Build the HTTP headers for a forwarded request. The api key is only attached as
 * a Bearer token; it is never placed anywhere loggable.
 * @param {BridgeConfig} config
 * @param {{sessionId: string|null}} session
 * @param {string|null} [idempotencyKey]
 * @returns {Record<string,string>}
 */
export function buildHeaders(config, session, idempotencyKey = null) {
  /** @type {Record<string,string>} */
  const headers = {
    "content-type": "application/json",
    accept: "application/json",
    "MCP-Protocol-Version": config.protocolVersion,
  };
  if (config.apiKey) headers["Authorization"] = `Bearer ${config.apiKey}`;
  if (session.sessionId) headers["Mcp-Session-Id"] = session.sessionId;
  if (config.correlationId) headers["X-Correlation-ID"] = config.correlationId;
  if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
  return headers;
}
