#!/usr/bin/env node
// Kenwea MCP bridge — CLI entry point.
//
// Usage:
//   kenwea-mcp [proxy]      run the stdio<->HTTP bridge (default; this is what an
//                           MCP client spawns)
//   kenwea-mcp init         print ready-to-paste client configuration
//   kenwea-mcp doctor       run a connectivity + auth check
//   kenwea-mcp check <url>  get a third-party sandbox verdict on an artifact
//
// Flags (override env): --url <endpoint>  --key <agentKey>  --protocol <version>
//   --help, -h   show usage
//   --version, -v show version

import { readFileSync } from "node:fs";

import { resolveConfig, validateConfig } from "./config.js";
import { runProxy } from "./proxy.js";
import { runDoctor } from "./doctor.js";
import { runInit } from "./init.js";
import { runCheck } from "./check.js";

// Read from package.json rather than restated here.
//
// The comment that used to sit on this line said "kept in sync with package.json",
// and it was not: two releases shipped as 0.1.2 while `kenwea-mcp --version`
// answered 0.1.0, because nothing compared the two. A constant that has to be
// remembered is a constant that drifts, and the drift is invisible precisely to
// the person asking which version they are running.
const VERSION = JSON.parse(
  readFileSync(new URL("../package.json", import.meta.url), "utf8"),
).version;

const USAGE = `kenwea-mcp — third-party sandbox verdicts, and an MCP bridge to Kenwea

Usage:
  kenwea-mcp check <pkg|url>  Notarize what an artifact does. Give an npm package
                              (express, express@4.18.2, @scope/pkg@1.2.3) or an https
                              URL. Kenwea fetches it, runs it in a sandbox with no
                              network, no capabilities and a read-only filesystem, and
                              prints a signed verdict. Mints a free key on first use --
                              no signup, no payment.
  kenwea-mcp doctor           Check connectivity and authentication.
  kenwea-mcp init             Print ready-to-paste client configuration.
  kenwea-mcp [proxy]          Run the stdio<->HTTP bridge (default). MCP clients spawn this.

Options:
  --fail-on <verdict>    Exit non-zero when the verdict is at or past this level, so a
                         CI job can gate on it. One of: manual_review, rejected.
                         Default: report only, always exit 0 when the check ran.
  --json                 Print the raw result as JSON on stdout (progress goes to
                         stderr), for scripts and CI. Works with --fail-on.
  --url <endpoint>       Override the remote endpoint (env: KENWEA_MCP_URL)
  --key <agentKey>       Bearer agent key (env: KENWEA_API_KEY)
  --protocol <version>   MCP-Protocol-Version (env: KENWEA_MCP_PROTOCOL_VERSION)
  -h, --help             Show this help
  -v, --version          Show version

Examples:
  kenwea-mcp check express@4.18.2
  kenwea-mcp check @kenwea/mcp --fail-on rejected

Default endpoint: https://mcp.kenwea.com/mcp/v1
`;

/**
 * @param {string[]} argv the args after `node cli.js`
 * @returns {{command: string, overrides: Partial<import("./config.js").BridgeConfig>, help: boolean, version: boolean, artifactRef: string|null, failOn: string|null, json: boolean}}
 */
export function parseArgs(argv) {
  /** @type {Partial<import("./config.js").BridgeConfig>} */
  const overrides = {};
  let command = "proxy";
  /** @type {string|null} */
  let artifactRef = null;
  /** @type {string|null} */
  let failOn = null;
  let json = false;
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
      case "--fail-on":
        failOn = argv[++i];
        break;
      case "--json":
        json = true;
        break;
      case "--protocol":
        overrides.protocolVersion = argv[++i];
        break;
      case "proxy":
      case "init":
      case "doctor":
      case "check":
        command = arg;
        break;
      default:
        // The first bare word picks the command; any later one is the check
        // target. Without this second clause `check https://x/y.js` parsed the
        // URL AS the command and silently ran the proxy instead -- a wrong
        // command that looks like a hang.
        if (!arg || arg.startsWith("-")) break;
        if (command === "proxy") command = arg;
        else if (artifactRef === null) artifactRef = arg;
        break;
    }
  }
  return { command, overrides, help, version, artifactRef, failOn, json };
}

/**
 * @param {string[]} argv process.argv.slice(2)
 * @param {NodeJS.ProcessEnv} env
 * @returns {Promise<number>} process exit code
 */
export async function main(argv, env) {
  const { command, overrides, help, version, artifactRef, failOn, json } = parseArgs(argv);

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
    case "check": {
      if (failOn !== null && failOn !== "manual_review" && failOn !== "rejected") {
        process.stderr.write(`[kenwea-mcp] --fail-on must be one of: manual_review, rejected (got "${failOn}")\n`);
        return 2;
      }
      const { ok } = await runCheck(config, artifactRef ?? "", {}, { failOn: failOn ?? undefined, json });
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
