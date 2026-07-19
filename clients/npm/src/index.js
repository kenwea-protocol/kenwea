// Kenwea MCP bridge — programmatic entry point.
//
// Re-exports the pure building blocks so the bridge can be embedded in other
// Node programs, not only run as a CLI.

export {
  DEFAULT_URL,
  DEFAULT_PROTOCOL_VERSION,
  SUPPORTED_PROTOCOL_VERSIONS,
  resolveConfig,
  validateConfig,
  redact,
} from "./config.js";

export {
  IDEMPOTENT_TOOLS,
  isNotification,
  toolNameOf,
  needsIdempotency,
  explicitIdempotencyKey,
  buildHeaders,
} from "./protocol.js";

export { runProxy, forwardOne, writeMessage } from "./proxy.js";
export { runDoctor, deriveHealthUrl } from "./doctor.js";
export { runInit, buildClientConfig } from "./init.js";
