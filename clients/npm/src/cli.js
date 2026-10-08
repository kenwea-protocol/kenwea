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
import { runCheck, normaliseGates, GATES, EXIT } from "./check.js";

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
                              URL. Kenwea fetches it and runs it in a sandbox with no
                              network, no capabilities, a read-only filesystem and no
                              root; for a package it runs the install steps npm would
                              run and traces what they attempt. No key, no signup.
                              The signed record is verified here before it is shown.
  kenwea-mcp doctor           Check connectivity and authentication.
  kenwea-mcp init             Print ready-to-paste client configuration.
  kenwea-mcp [proxy]          Run the stdio<->HTTP bridge (default). MCP clients spawn this.

Options for check:
  --fail-on <gates>      Fail the run when the signed record trips a gate, so a CI
                         job can block on it. Comma separated, any of:
                           install-scripts  the package runs anything at install
                           network          an install step tried to reach the network
                           manual_review    the verdict is manual_review or rejected
                           rejected         the verdict is rejected
                         A gate that gets no answer, or one that does not verify,
                         fails closed. Default: report only.
  --json                 Print the raw result as JSON on stdout (progress goes to
                         stderr), for scripts and CI. Works with --fail-on.
  --no-verify            Skip verifying the record here. Only for a server you run
                         yourself without Kenwea's key.

Options:
  --url <endpoint>       Override the remote endpoint (env: KENWEA_MCP_URL)
  --key <agentKey>       Bearer agent key (env: KENWEA_API_KEY)
  --protocol <version>   MCP-Protocol-Version (env: KENWEA_MCP_PROTOCOL_VERSION)
  -h, --help             Show this help
  -v, --version          Show version

Exit codes for check:
  0  checked and verified, and no gate tripped (or none was set)
  1  a --fail-on gate tripped
  2  the command line was wrong
  3  no answer: not resolvable, not fetchable, refused or unreachable
  4  the answer did not verify: bad or revoked signature, or not about these bytes

Examples:
  kenwea-mcp check express@4.18.2
  kenwea-mcp check @kenwea/mcp --fail-on install-scripts,network

Default endpoint: https://mcp.kenwea.com/mcp/v1
`;

const COMMANDS = ["proxy", "init", "doctor", "check"];

/**
 * @param {string[]} argv the args after `node cli.js`
 * @returns {{command: string, overrides: Partial<import("./config.js").BridgeConfig>, help: boolean, version: boolean, artifactRef: string|null, failOn: string|null, json: boolean, verify: boolean, errors: string[]}}
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
  let verify = true;
  let help = false;
  let version = false;
  let commandSeen = false;
  /** @type {string[]} */
  const errors = [];
  // A flag that takes a value must get one. Until 0.3.0 `--fail-on` at the end of
  // a command line read undefined and the gate silently did not exist.
  const value = (/** @type {number} */ i, /** @type {string} */ flag) => {
    const v = argv[i];
    if (v === undefined || v.startsWith("-")) {
      errors.push(`${flag} needs a value`);
      return null;
    }
    return v;
  };

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
        overrides.url = value(++i, arg) ?? undefined;
        break;
      case "--key":
        overrides.apiKey = value(++i, arg) ?? undefined;
        break;
      case "--fail-on":
        failOn = value(++i, arg);
        break;
      case "--json":
        json = true;
        break;
      case "--no-verify":
        verify = false;
        break;
      case "--protocol":
        overrides.protocolVersion = value(++i, arg) ?? undefined;
        break;
      default:
        if (!arg) break;
        // An option this version does not know is an error, not a no-op: a gate
        // that was misspelt must not pass a build.
        if (arg.startsWith("-")) {
          errors.push(`unknown option "${arg}"`);
          break;
        }
        // The first bare word picks the command; the next one is the check
        // target. Without this second clause `check https://x/y.js` parsed the
        // URL AS the command and silently ran the proxy instead -- a wrong
        // command that looks like a hang.
        if (!commandSeen) {
          if (!COMMANDS.includes(arg)) errors.push(`unknown command "${arg}"`);
          else command = arg;
          commandSeen = true;
        } else if (command === "check" && artifactRef === null) artifactRef = arg;
        else errors.push(`unexpected argument "${arg}"`);
        break;
    }
  }
  return { command, overrides, help, version, artifactRef, failOn, json, verify, errors };
}

/**
 * @param {string[]} argv process.argv.slice(2)
 * @param {NodeJS.ProcessEnv} env
 * @returns {Promise<number>} process exit code
 */
export async function main(argv, env) {
  const { command, overrides, help, version, artifactRef, failOn, json, verify, errors } = parseArgs(argv);

  if (errors.length && !help && !version) {
    for (const e of errors) process.stderr.write(`[kenwea-mcp] ${e}\n`);
    process.stderr.write("[kenwea-mcp] run kenwea-mcp --help for usage\n");
    return EXIT.USAGE;
  }
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
      const gates = normaliseGates(failOn ?? undefined);
      const unknown = gates.filter((g) => !GATES.includes(g));
      if (failOn !== null && (unknown.length || !gates.length)) {
        process.stderr.write(`[kenwea-mcp] --fail-on takes one or more of: ${GATES.join(", ")} (got "${failOn}")\n`);
        return EXIT.USAGE;
      }
      const { code } = await runCheck(config, artifactRef ?? "", {}, { failOn: gates, json, verify });
      return code;
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
