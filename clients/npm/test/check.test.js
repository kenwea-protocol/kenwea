import assert from "node:assert/strict";
import test from "node:test";

import { parseArgs } from "../src/cli.js";
import { runCheck } from "../src/check.js";

const CONFIG = {
  url: "https://mcp.example/mcp/v1",
  apiKey: "",
  protocolVersion: "2025-11-25",
  correlationId: "",
};

function sink() {
  /** @type {string[]} */
  const chunks = [];
  return { write: (/** @type {string} */ s) => chunks.push(s), text: () => chunks.join("") };
}

/**
 * A fetch double that answers each POST from a queue, keyed by nothing but order.
 * @param {any[]} responses
 */
function fetchQueue(responses) {
  /** @type {any[]} */
  const calls = [];
  const queue = [...responses];
  return {
    calls,
    impl: async (/** @type {any} */ url, /** @type {any} */ init) => {
      calls.push(JSON.parse(init.body));
      const body = queue.shift() ?? { jsonrpc: "2.0", id: 1, result: {} };
      return {
        ok: true,
        status: 200,
        headers: { get: () => null },
        text: async () => JSON.stringify(body),
      };
    },
  };
}

/**
 * The doubles above are deliberately partial: a fetch stand-in that returns four
 * fields, and a writable that only collects strings. Casting here rather than
 * loosening runCheck's own JSDoc keeps the production signature honest -- the test
 * is what is approximate, not the function under test.
 * @param {any} fetchImpl
 * @param {any} stdout
 * @param {any} stderr
 * @returns {any}
 */
function io(fetchImpl, stdout, stderr) {
  return { fetchImpl, stdout, stderr };
}

const toolResult = (/** @type {any} */ payload) => ({
  jsonrpc: "2.0",
  id: 1,
  result: { content: [{ type: "text", text: JSON.stringify(payload) }] },
});

// The bug this catches was live for one edit: `check` and the URL are both bare
// words, and the original parser let the LAST bare word win, so
// `check https://x/y.js` parsed the URL as the command and ran the proxy instead.
// A wrong command that looks like a hang is the worst possible failure for the
// first command a stranger ever types.
test("parseArgs keeps the command and the artifact separate", () => {
  const parsed = parseArgs(["check", "https://example.com/tool.js"]);
  assert.equal(parsed.command, "check");
  assert.equal(parsed.artifactRef, "https://example.com/tool.js");
});

test("parseArgs still honours flags around the artifact", () => {
  const parsed = parseArgs(["check", "https://example.com/t.js", "--key", "kw_x"]);
  assert.equal(parsed.command, "check");
  assert.equal(parsed.artifactRef, "https://example.com/t.js");
  assert.equal(parsed.overrides.apiKey, "kw_x");
});

test("a non-https artifact is refused locally, before any request is made", async () => {
  const q = fetchQueue([]);
  const err = sink();
  const res = await runCheck(CONFIG, "ftp://example.com/x.js", io(q.impl, sink(), err));
  assert.equal(res.ok, false);
  assert.equal(q.calls.length, 0, "must not spend a network round trip on an input we can already reject");
  assert.match(err.text(), /https URL/);
});

// The whole point of the command: someone who has never heard of Kenwea types one
// line and gets a verdict. If it ever demands a key first, it has lost the person
// it exists for.
test("with no key it mints one, says so, and shows it", async () => {
  const q = fetchQueue([
    { jsonrpc: "2.0", id: 1, result: {} }, // initialize
    toolResult({ apiKey: { rawKey: "kw_agent_minted" } }), // registerSelf
    toolResult({ checked: true, verdict: "approved", ran: true, exitCode: 0, contentSha256: "abc" }),
  ]);
  const out = sink();
  const res = await runCheck(CONFIG, "https://example.com/tool.js", io(q.impl, out, sink()));

  assert.equal(res.ok, true);
  assert.equal(res.verdict, "approved");
  assert.equal(q.calls[1].params.name, "kenwea.onboarding.registerSelf");
  assert.equal(q.calls[1].params.arguments.agentName, "kenwea-mcp-check", "the field is agentName, not name");
  assert.equal(q.calls[2].params.name, "kenwea.sandbox.check");
  // A credential created on someone's behalf and never shown to them is exactly
  // the thing this project refuses to do elsewhere.
  assert.match(out.text(), /kw_agent_minted/);
});

test("an existing key is used as-is and no agent is created", async () => {
  const q = fetchQueue([
    { jsonrpc: "2.0", id: 1, result: {} },
    toolResult({ checked: true, verdict: "approved", ran: true, exitCode: 0 }),
  ]);
  await runCheck({ ...CONFIG, apiKey: "kw_existing" }, "https://example.com/tool.js", io(q.impl, sink(), sink()));
  const registered = q.calls.some((c) => c.params && c.params.name === "kenwea.onboarding.registerSelf");
  assert.equal(registered, false, "a caller who brought a key must not have a second agent minted for them");
});

// An honest non-answer is not a failure. Exiting non-zero here would train callers
// to read "we could not fetch it" as "the artifact is bad" -- which is the single
// most damaging thing a verdict tool can do.
test("an unfetchable artifact reports why and does not claim a verdict", async () => {
  const q = fetchQueue([
    { jsonrpc: "2.0", id: 1, result: {} },
    toolResult({ checked: false, reason: "scheme_not_fetchable", note: "nothing was executed" }),
  ]);
  const out = sink();
  const res = await runCheck({ ...CONFIG, apiKey: "kw_existing" }, "https://example.com/x", io(q.impl, out, sink()));
  assert.equal(res.ok, true);
  assert.equal(res.verdict, null);
  assert.match(out.text(), /NOT CHECKED/);
  assert.doesNotMatch(out.text(), /APPROVED/);
});

test("a rejected verdict is rendered with its reason and the sandbox output", async () => {
  const q = fetchQueue([
    { jsonrpc: "2.0", id: 1, result: {} },
    toolResult({
      checked: true,
      verdict: "rejected",
      verdictReason: "the artifact ran and exited non-zero",
      ran: true,
      exitCode: 1,
      executable: "node",
      output: "Error: Cannot find module './src/common'",
    }),
  ]);
  const out = sink();
  const res = await runCheck({ ...CONFIG, apiKey: "kw_existing" }, "https://example.com/x.js", io(q.impl, out, sink()));
  assert.equal(res.ok, true);
  assert.equal(res.verdict, "rejected");
  assert.match(out.text(), /REJECTED/);
  assert.match(out.text(), /exited non-zero/);
  assert.match(out.text(), /Cannot find module/, "a verdict without the reason behind it is not actionable");
});

// --- npm package resolution: a developer types a name, not a tarball URL ---

/**
 * A fetch double that answers the registry GET with a manifest and the MCP POSTs
 * from a queue. Branches on method so a bodyless GET never hits JSON.parse.
 * @param {string} tarball  the dist.tarball the registry should report
 * @param {any[]} mcp       ordered MCP responses
 */
function registryAndMcp(tarball, mcp) {
  /** @type {any[]} */
  const posts = [];
  /** @type {string[]} */
  const gets = [];
  const queue = [...mcp];
  const impl = async (/** @type {any} */ url, /** @type {any} */ init) => {
    if (!init || !init.body) {
      gets.push(String(url));
      return {
        ok: true,
        status: 200,
        headers: { get: () => null },
        json: async () => ({ name: "express", version: "4.18.2", dist: { tarball } }),
      };
    }
    posts.push(JSON.parse(init.body));
    const body = queue.shift() ?? { jsonrpc: "2.0", id: 1, result: {} };
    return { ok: true, status: 200, headers: { get: () => null }, text: async () => JSON.stringify(body) };
  };
  return { posts, gets, impl };
}

test("a bare package name is resolved to its registry tarball, then checked", async () => {
  const tarball = "https://registry.npmjs.org/express/-/express-4.18.2.tgz";
  const d = registryAndMcp(tarball, [
    { jsonrpc: "2.0", id: 1, result: {} }, // initialize
    toolResult({ checked: true, verdict: "approved", ran: true, exitCode: 0, contentSha256: "abc" }),
  ]);
  const out = sink();
  const res = await runCheck({ ...CONFIG, apiKey: "kw_existing" }, "express@4.18.2", io(d.impl, out, sink()));
  assert.equal(res.ok, true);
  assert.equal(d.gets.length, 1, "must ask the registry exactly once");
  assert.match(d.gets[0], /registry\.npmjs\.org\/express\/4\.18\.2/);
  // The sandbox call must carry the RESOLVED tarball, not the bare spec.
  const sandboxCall = d.posts.find((m) => m.params && m.params.name === "kenwea.sandbox.check");
  assert.equal(sandboxCall.params.arguments.artifactRef, tarball);
  assert.match(out.text(), /Resolved express@4\.18\.2/, "the bytes under the verdict must not be a mystery");
});

test("a malformed non-https URL is rejected locally, no registry call", async () => {
  const d = registryAndMcp("https://x/y.tgz", []);
  const err = sink();
  const res = await runCheck({ ...CONFIG, apiKey: "kw_existing" }, "ftp://example.com/x.js", io(d.impl, sink(), err));
  assert.equal(res.ok, false);
  assert.equal(d.gets.length, 0);
  assert.equal(d.posts.length, 0);
  assert.match(err.text(), /npm package name/);
});

// --- CI gate: --fail-on turns a verdict into an exit code ---

/** @param {string} verdict @param {string|undefined} failOn */
async function checkWithVerdict(verdict, failOn) {
  const q = fetchQueue([
    { jsonrpc: "2.0", id: 1, result: {} },
    toolResult({ checked: true, verdict, ran: verdict !== "notarized", contentSha256: "abc" }),
  ]);
  return runCheck({ ...CONFIG, apiKey: "kw_existing" }, "https://example.com/x.js", io(q.impl, sink(), sink()), { failOn });
}

test("without --fail-on, even a rejected verdict exits 0 (reporting is success)", async () => {
  const res = await checkWithVerdict("rejected", undefined);
  assert.equal(res.ok, true);
});

test("--fail-on rejected fails on rejected but not on manual_review", async () => {
  assert.equal((await checkWithVerdict("rejected", "rejected")).ok, false);
  assert.equal((await checkWithVerdict("manual_review", "rejected")).ok, true);
  assert.equal((await checkWithVerdict("approved", "rejected")).ok, true);
});

test("--fail-on manual_review also fails on rejected, not on a clean verdict", async () => {
  assert.equal((await checkWithVerdict("manual_review", "manual_review")).ok, false);
  assert.equal((await checkWithVerdict("rejected", "manual_review")).ok, false);
  assert.equal((await checkWithVerdict("approved", "manual_review")).ok, true);
  assert.equal((await checkWithVerdict("notarized", "manual_review")).ok, true);
});

test("an honest non-answer (checked=false) never trips the gate", async () => {
  const q = fetchQueue([
    { jsonrpc: "2.0", id: 1, result: {} },
    toolResult({ checked: false, reason: "the artifact was not retrieved" }),
  ]);
  const res = await runCheck({ ...CONFIG, apiKey: "kw_existing" }, "https://example.com/x.js", io(q.impl, sink(), sink()), { failOn: "manual_review" });
  assert.equal(res.ok, true, "we could not read it must never become a red build");
});

// --- --json: machine-readable stdout for scripts and the GitHub Action ---

test("--json prints only the result object on stdout, notices on stderr", async () => {
  const d = registryAndMcp("https://registry.npmjs.org/express/-/express-4.18.2.tgz", [
    { jsonrpc: "2.0", id: 1, result: {} },
    toolResult({ checked: true, verdict: "approved", ran: true, exitCode: 0, contentSha256: "abc123" }),
  ]);
  const out = sink();
  const err = sink();
  const res = await runCheck({ ...CONFIG, apiKey: "kw_existing" }, "express@4.18.2", io(d.impl, out, err), { json: true });
  assert.equal(res.ok, true);
  // stdout must parse as exactly the result object -- nothing else on it.
  const parsed = JSON.parse(out.text());
  assert.equal(parsed.verdict, "approved");
  assert.equal(parsed.contentSha256, "abc123");
  // the "Resolved …" notice must be on stderr, or it would corrupt the JSON.
  assert.match(err.text(), /Resolved express/);
  assert.doesNotMatch(out.text(), /Resolved/);
});

test("--json still honours --fail-on for the exit code", async () => {
  const q = fetchQueue([
    { jsonrpc: "2.0", id: 1, result: {} },
    toolResult({ checked: true, verdict: "rejected", ran: true, exitCode: 1, contentSha256: "abc" }),
  ]);
  const out = sink();
  const res = await runCheck({ ...CONFIG, apiKey: "kw_existing" }, "https://example.com/x.js", io(q.impl, out, sink()), { json: true, failOn: "rejected" });
  assert.equal(res.ok, false, "the gate still fires in json mode");
  assert.equal(JSON.parse(out.text()).verdict, "rejected", "and the JSON is still emitted");
});
