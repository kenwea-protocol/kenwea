#!/usr/bin/env node
// Kenwea MCP bridge — CLI entry point.
//
// Usage:
//   kenwea-mcp [proxy]      run the stdio<->HTTP bridge (default; this is what an
//                           MCP client spawns)
//   kenwea-mcp init         print ready-to-paste client configuration
//   kenwea-mcp doctor       run a connectivity + auth check
//
// Flags (override env): --url <endpoint>  --key <agentKey>  --protocol <version>
//   --help, -h   show usage
//   --version, -v show version

import { resolveConfig, validateConfig } from "./config.js";
import { runProxy } from "./proxy.js";
import { runDoctor } from "./doctor.js";
import { runInit } from "./init.js";

// Kept in sync with package.json "version".
const VERSION = "0.1.0";

const USAGE = `kenwea-mcp — one-line MCP bridge to the Kenwea agent marketplace

Usage:
  kenwea-mcp [proxy]     Run the stdio<->HTTP bridge (default). MCP clients spawn this.
  kenwea-mcp init        Print ready-to-paste client configuration.
  kenwea-mcp doctor      Check connectivity and authentication.

Options:
  --url <endpoint>       Override the remote endpoint (env: KENWEA_MCP_URL)
  --key <agentKey>       Bearer agent key (env: KENWEA_API_KEY)
  --protocol <version>   MCP-Protocol-Version (env: KENWEA_MCP_PROTOCOL_VERSION)
  -h, --help             Show this help
  -v, --version          Show version

Default endpoint: https://mcp.kenwea.com/mcp/v1
`;

/**
 * @param {string[]} argv the args after `node cli.js`
 * @returns {{command: string, overrides: Partial<import("./config.js").BridgeConfig>, help: boolean, version: boolean}}
 */
export function parseArgs(argv) {
  /** @type {Partial<import("./config.js").BridgeConfig>} */
  const overrides = {};
  let command = "proxy";
  let help = false;
  let version = false;

  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    switch (arg) {
      case "-h":
      case "--help":
        help = true;
        break;
      case "-v":
      case "--version":
        version = true;
        break;
      case "--url":
        overrides.url = argv[++i];
        break;
      case "--key":
        overrides.apiKey = argv[++i];
        break;
      case "--protocol":
        overrides.protocolVersion = argv[++i];
        break;
      case "proxy":
      case "init":
      case "doctor":
        command = arg;
        break;
      default:
        if (arg && !arg.startsWith("-")) command = arg;
        break;
    }
  }
  return { command, overrides, help, version };
}

/**
 * @param {string[]} argv process.argv.slice(2)
 * @param {NodeJS.ProcessEnv} env
 * @returns {Promise<number>} process exit code
 */
export async function main(argv, env) {
  const { command, overrides, help, version } = parseArgs(argv);

  if (help) {
    process.stdout.write(USAGE);
    return 0;
  }
  if (version) {
    process.stdout.write(VERSION + "\n");
    return 0;
  }

  const config = resolveConfig(env, overrides);
  const invalid = validateConfig(config);
  if (invalid) {
    process.stderr.write(`[kenwea-mcp] ${invalid}\n`);
    return 2;
  }

  switch (command) {
    case "init":
      runInit(config);
      return 0;
    case "doctor": {
      const { ok } = await runDoctor(config);
      return ok ? 0 : 1;
    }
    case "proxy":
    default:
      await runProxy(config);
      return 0;
  }
}

// Only run when invoked as a program (not when imported by tests).
const invokedDirectly =
  process.argv[1] && import.meta.url === `file://${process.argv[1].replace(/\\/g, "/")}`;
const invokedViaBin =
  process.argv[1] && /(?:^|[\\/])(?:kenwea-mcp|kenwea|cli\.js)$/.test(process.argv[1]);

if (invokedDirectly || invokedViaBin) {
  main(process.argv.slice(2), process.env)
    .then((code) => {
      process.exitCode = code;
    })
    .catch((err) => {
      process.stderr.write(`[kenwea-mcp] fatal: ${String(err && err.message ? err.message : err)}\n`);
      process.exitCode = 1;
    });
}
