// Kenwea MCP bridge — configuration resolution.
//
// Pure, side-effect-free config helpers. All network/stdio I/O lives elsewhere so
// this module can be unit-tested without touching the environment.

export const DEFAULT_URL = "https://mcp.kenwea.com/mcp/v1";
export const DEFAULT_PROTOCOL_VERSION = "2025-11-25";
export const SUPPORTED_PROTOCOL_VERSIONS = ["2025-11-25", "2025-03-26"];

/**
 * @typedef {Object} BridgeConfig
 * @property {string} url               Remote Kenwea MCP endpoint (Streamable HTTP).
 * @property {string|null} apiKey       Bearer agent key. Without one, only `initialize`,
 *                                      `tools/list`, and `registerSelf` work — all other
 *                                      tools return `unauthorized`.
 * @property {string} protocolVersion   MCP-Protocol-Version header value.
 * @property {string|null} correlationId Optional trace id forwarded as X-Correlation-ID.
 */

/**
 * Resolve the effective bridge configuration from environment plus explicit overrides.
 * Overrides (CLI flags) win over environment, which wins over defaults.
 *
 * @param {NodeJS.ProcessEnv} env
 * @param {Partial<BridgeConfig>} [overrides]
 * @returns {BridgeConfig}
 */
export function resolveConfig(env, overrides = {}) {
  const url = overrides.url ?? env.KENWEA_MCP_URL ?? DEFAULT_URL;
  const apiKey =
    overrides.apiKey ?? env.KENWEA_API_KEY ?? env.KENWEA_AGENT_KEY ?? null;
  const protocolVersion =
    overrides.protocolVersion ??
    env.KENWEA_MCP_PROTOCOL_VERSION ??
    DEFAULT_PROTOCOL_VERSION;
  const correlationId =
    overrides.correlationId ?? env.KENWEA_CORRELATION_ID ?? null;
  return { url, apiKey, protocolVersion, correlationId };
}

/**
 * Validate a resolved config, returning a human-readable error string or null.
 * @param {BridgeConfig} config
 * @returns {string|null}
 */
export function validateConfig(config) {
  let parsed;
  try {
    parsed = new URL(config.url);
  } catch {
    return `invalid KENWEA_MCP_URL: ${config.url}`;
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
    return `KENWEA_MCP_URL must be http(s): ${config.url}`;
  }
  if (!SUPPORTED_PROTOCOL_VERSIONS.includes(config.protocolVersion)) {
    return `unsupported MCP-Protocol-Version: ${config.protocolVersion} (supported: ${SUPPORTED_PROTOCOL_VERSIONS.join(", ")})`;
  }
  return null;
}

/**
 * Redact the api key from an arbitrary string so it never reaches logs.
 * @param {string} text
 * @param {BridgeConfig} config
 * @returns {string}
 */
export function redact(text, config) {
  if (!config.apiKey) return text;
  return text.split(config.apiKey).join("***");
}
