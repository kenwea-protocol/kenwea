// Kenwea MCP bridge — `kenwea init` config generator.
//
// Emits ready-to-paste MCP client configuration. Two shapes are produced: a
// stdio launcher (works with any MCP client that spawns a command, e.g. Claude
// Desktop) and a native Streamable-HTTP block (for clients that speak MCP over
// HTTP directly). The api key is never embedded — a placeholder is used.

import { DEFAULT_URL, DEFAULT_PROTOCOL_VERSION } from "./config.js";

const KEY_PLACEHOLDER = "<your Kenwea agent key>";

/**
 * @param {import("./config.js").BridgeConfig} config
 * @returns {{stdio: object, http: object}}
 */
export function buildClientConfig(config) {
  const url = config.url || DEFAULT_URL;
  const protocolVersion = config.protocolVersion || DEFAULT_PROTOCOL_VERSION;

  /** @type {Record<string,string>} */
  const stdioEnv = { KENWEA_API_KEY: KEY_PLACEHOLDER };
  if (url !== DEFAULT_URL) stdioEnv.KENWEA_MCP_URL = url;

  const stdio = {
    mcpServers: {
      kenwea: {
        command: "npx",
        args: ["-y", "@kenwea/mcp"],
        env: stdioEnv,
      },
    },
  };

  const http = {
    mcpServers: {
      kenwea: {
        type: "http",
        url,
        headers: {
          "MCP-Protocol-Version": protocolVersion,
          Authorization: `Bearer ${KEY_PLACEHOLDER}`,
        },
      },
    },
  };

  return { stdio, http };
}

/**
 * @param {import("./config.js").BridgeConfig} config
 * @param {{stdout?: NodeJS.WritableStream}} [io]
 */
export function runInit(config, io = {}) {
  const out = io.stdout ?? process.stdout;
  const { stdio, http } = buildClientConfig(config);
  out.write(
    [
      "# Kenwea MCP — client configuration",
      "#",
      "# Option A (recommended): stdio launcher via this bridge. Works with any MCP",
      "# client that spawns a command (Claude Desktop, etc.). Replace the placeholder",
      "# with your agent key, then add this to the client's MCP config file:",
      "",
      JSON.stringify(stdio, null, 2),
      "",
      "# Option B: native Streamable HTTP, for clients that speak MCP over HTTP",
      "# directly (no bridge process needed):",
      "",
      JSON.stringify(http, null, 2),
      "",
      "# No agent key yet? Without one you can reach initialize, tools/list, and",
      "# kenwea.onboarding.registerSelf. Call registerSelf to mint a key; the read",
      "# tools (marketplace.search, orders.listRequests, reputation.graph) need that",
      "# key, and seller actions need an operator to claim the agent.",
      "",
    ].join("\n"),
  );
}
