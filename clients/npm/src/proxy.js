// Kenwea MCP bridge — the stdio <-> Streamable-HTTP proxy.
//
// Reads newline-delimited JSON-RPC messages from stdin (the MCP stdio transport
// framing), forwards each to the remote Kenwea MCP endpoint over HTTP, captures
// the issued Mcp-Session-Id, and writes each response back to stdout. Notifications
// (no id) receive no response, matching the server's 202-no-body behaviour.

import { createInterface } from "node:readline";
import { randomUUID } from "node:crypto";
import {
  buildHeaders,
  isNotification,
  needsIdempotency,
  explicitIdempotencyKey,
} from "./protocol.js";
import { redact } from "./config.js";

/**
 * @typedef {import("./config.js").BridgeConfig} BridgeConfig
 */

/**
 * @typedef {Object} ProxyIO
 * @property {NodeJS.ReadableStream} [stdin]
 * @property {NodeJS.WritableStream} [stdout]
 * @property {NodeJS.WritableStream} [stderr]
 * @property {typeof fetch} [fetchImpl]
 * @property {() => string} [generateId]
 */

/**
 * Write a single JSON-RPC message to a stream using MCP stdio framing (one
 * compact JSON object per line).
 * @param {NodeJS.WritableStream} stream
 * @param {unknown} message
 */
export function writeMessage(stream, message) {
  stream.write(JSON.stringify(message) + "\n");
}

/**
 * Forward exactly one parsed JSON-RPC message and return the response object to
 * emit, or null when nothing should be written (notifications).
 *
 * @param {any} msg
 * @param {BridgeConfig} config
 * @param {{sessionId: string|null}} session   mutated in place with any issued session id
 * @param {typeof fetch} doFetch
 * @param {() => string} genId
 * @returns {Promise<object|null>}
 */
export async function forwardOne(msg, config, session, doFetch, genId) {
  /** @type {string|null} */
  let idempotencyKey = null;
  if (needsIdempotency(msg)) {
    idempotencyKey = explicitIdempotencyKey(msg) ?? genId();
  }

  const headers = buildHeaders(config, session, idempotencyKey);
  const res = await doFetch(config.url, {
    method: "POST",
    headers,
    body: JSON.stringify(msg),
  });

  const issued = res.headers.get("Mcp-Session-Id");
  if (issued) session.sessionId = issued;

  const notification = isNotification(msg);
  const text = await res.text();
  if (notification) return null;

  const id = msg && typeof msg === "object" && msg.id !== undefined ? msg.id : null;
  if (!text) {
    return {
      jsonrpc: "2.0",
      id,
      error: {
        code: -32002,
        message: "empty_upstream_response",
        data: { status: res.status },
      },
    };
  }
  try {
    return JSON.parse(text);
  } catch {
    return {
      jsonrpc: "2.0",
      id,
      error: {
        code: -32002,
        message: "invalid_upstream_json",
        data: { status: res.status },
      },
    };
  }
}

/**
 * Run the bridge until stdin closes. Resolves when the input stream ends.
 * @param {BridgeConfig} config
 * @param {ProxyIO} [io]
 * @returns {Promise<void>}
 */
export async function runProxy(config, io = {}) {
  const stdin = io.stdin ?? process.stdin;
  const stdout = io.stdout ?? process.stdout;
  const stderr = io.stderr ?? process.stderr;
  const doFetch = io.fetchImpl ?? fetch;
  const genId = io.generateId ?? randomUUID;

  /** @type {{sessionId: string|null}} */
  const session = { sessionId: null };

  const rl = createInterface({ input: stdin, crlfDelay: Infinity });
  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;

    /** @type {any} */
    let msg;
    try {
      msg = JSON.parse(trimmed);
    } catch {
      writeMessage(stdout, {
        jsonrpc: "2.0",
        id: null,
        error: { code: -32700, message: "parse_error" },
      });
      continue;
    }

    try {
      const response = await forwardOne(msg, config, session, doFetch, genId);
      if (response !== null) writeMessage(stdout, response);
    } catch (err) {
      const detail = redact(
        String((err && /** @type {any} */ (err).message) || err),
        config,
      );
      const hasId = msg && typeof msg === "object" && msg.id !== undefined;
      if (!isNotification(msg) && hasId) {
        writeMessage(stdout, {
          jsonrpc: "2.0",
          id: msg.id,
          error: {
            code: -32001,
            message: "bridge_transport_error",
            data: { detail },
          },
        });
      } else {
        stderr.write(`[kenwea-mcp] transport error: ${detail}\n`);
      }
    }
  }
}
