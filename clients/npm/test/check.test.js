import assert from "node:assert/strict";
import { createHash, generateKeyPairSync, sign } from "node:crypto";
import test from "node:test";

import { parseArgs } from "../src/cli.js";
import { EXIT, KEYS_URL, gateRefusals, runCheck, sanitize } from "../src/check.js";

const CONFIG = {
  url: "https://mcp.example/mcp/v1",
  apiKey: "",
  protocolVersion: "2025-11-25",
  correlationId: "",
};
const KEYED = { ...CONFIG, apiKey: "kw_existing" };
const URL_REF = "https://example.com/tool.js";

// --- a signing harness: real Ed25519 keys, a published list, signed records ---

/** @param {string} status @param {string} [reason] */
function makeKey(status, reason) {
  const { publicKey, privateKey } = generateKeyPairSync("ed25519");
  const publicKeyBase64 = publicKey.export({ format: "der", type: "spki" }).subarray(-32).toString("base64");
  const keyId = createHash("sha256").update(publicKeyBase64).digest("hex").slice(0, 16);
  return { privateKey, entry: { keyId, algorithm: "ed25519", publicKeyBase64, status, ...(reason ? { reason } : {}) } };
}
const ACTIVE = makeKey("active");
const REVOKED = makeKey("revoked", "the seed was kept where it could have been copied");
const KEY_LIST = { keys: [ACTIVE.entry, REVOKED.entry] };

/**
 * A result the way the notary returns it: unsigned fields beside a signed payload.
 * @param {Record<string, any>} facts the signed facts (artifactRef defaults to URL_REF)
 * @param {{key?: any, format1?: boolean, unsigned?: Record<string, any>}} [o]
 */
function record(facts, o = {}) {
  const key = o.key ?? ACTIVE;
  const signedFacts = {
    issuer: "kenwea.com",
    ...(o.format1 ? {} : { format: 2, keyId: key.entry.keyId }),
    artifactRef: URL_REF,
    contentSha256: "ab".repeat(32),
    verdict: "approved",
    reasonCode: "ran_ok",
    ...facts,
  };
  const payload = JSON.stringify(signedFacts);
  return {
    checked: true,
    ran: true,
    exitCode: 0,
    ...signedFacts,
    ...(o.unsigned ?? {}),
    signedAttestation: {
      algorithm: "ed25519",
      keyId: key.entry.keyId,
      payload,
      signature: sign(null, Buffer.from(payload), key.privateKey).toString("base64"),
    },
  };
}

const toolResult = (/** @type {any} */ payload) => ({
  jsonrpc: "2.0",
  id: 1,
  result: { content: [{ type: "text", text: JSON.stringify(payload) }] },
});
const INIT = { jsonrpc: "2.0", id: 1, result: {} };

/**
 * One fetch double for everything the command reaches: MCP POSTs answered from a
 * queue, the key list, the registry manifest and the tarball by URL.
 * @param {any[]} mcp ordered MCP responses
 * @param {{keys?: any, manifest?: any, tarball?: Buffer}} [web]
 */
function network(mcp, web = {}) {
  /** @type {any[]} */
  const posts = [];
  /** @type {string[]} */
  const postUrls = [];
  /** @type {string[]} */
  const gets = [];
  const queue = [...mcp];
  const impl = async (/** @type {any} */ url, /** @type {any} */ init) => {
    const u = String(url);
    if (init && init.body) {
      posts.push(JSON.parse(init.body));
      postUrls.push(u);
      const body = queue.shift() ?? INIT;
      return { ok: true, status: 200, headers: { get: () => null }, text: async () => JSON.stringify(body) };
    }
    gets.push(u);
    if (u === KEYS_URL) return { ok: true, status: 200, text: async () => JSON.stringify(web.keys ?? KEY_LIST) };
    if (u.startsWith("https://registry.npmjs.org/") && !u.endsWith(".tgz")) {
      return { ok: !!web.manifest, status: web.manifest ? 200 : 404, text: async () => JSON.stringify(web.manifest ?? {}) };
    }
    if (web.tarball && u.endsWith(".tgz")) {
      const bytes = web.tarball;
      return { ok: true, status: 200, arrayBuffer: async () => bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) };
    }
    return { ok: false, status: 404, text: async () => "" };
  };
  return { posts, postUrls, gets, impl };
}

function sink() {
  /** @type {string[]} */
  const chunks = [];
  return { write: (/** @type {string} */ s) => chunks.push(s), text: () => chunks.join("") };
}

/**
 * The doubles are deliberately partial; casting here rather than loosening
 * runCheck's own JSDoc keeps the production signature honest.
 * @param {any} fetchImpl @param {any} stdout @param {any} stderr
 * @returns {any}
 */
function io(fetchImpl, stdout = sink(), stderr = sink()) {
  return { fetchImpl, stdout, stderr };
}

/** @param {any} result @param {any} [opts] @param {any} [config] */
async function check(result, opts = {}, config = KEYED) {
  const n = network([INIT, toolResult(result)]);
  const out = sink();
  const err = sink();
  const res = await runCheck(config, URL_REF, io(n.impl, out, err), opts);
  return { res, out: out.text(), err: err.text(), n };
}

// --- the command line ---

// The bug this catches was live for one edit: `check` and the URL are both bare
// words, and the original parser let the LAST bare word win, so
// `check https://x/y.js` parsed the URL as the command and ran the proxy instead.
test("parseArgs keeps the command and the artifact separate", () => {
  const parsed = parseArgs(["check", URL_REF]);
  assert.equal(parsed.command, "check");
  assert.equal(parsed.artifactRef, URL_REF);
  assert.deepEqual(parsed.errors, []);
});

test("parseArgs still honours flags around the artifact", () => {
  const parsed = parseArgs(["check", URL_REF, "--key", "kw_x", "--fail-on", "install-scripts,network", "--json"]);
  assert.equal(parsed.artifactRef, URL_REF);
  assert.equal(parsed.overrides.apiKey, "kw_x");
  assert.equal(parsed.failOn, "install-scripts,network");
  assert.equal(parsed.json, true);
  assert.deepEqual(parsed.errors, []);
});

// Until 0.3.0 an unknown option was ignored and a trailing --fail-on read
// undefined: a misspelt gate passed the build.
test("parseArgs refuses what it does not understand", () => {
  assert.match(parseArgs(["check", URL_REF, "--fail-onn", "rejected"]).errors.join(), /unknown option "--fail-onn"/);
  assert.match(parseArgs(["check", URL_REF, "--fail-on"]).errors.join(), /--fail-on needs a value/);
  assert.match(parseArgs(["check", URL_REF, "--fail-on", "--json"]).errors.join(), /--fail-on needs a value/);
  assert.match(parseArgs(["check", URL_REF, "extra"]).errors.join(), /unexpected argument "extra"/);
  assert.match(parseArgs(["chekc", URL_REF]).errors.join(), /unknown command "chekc"/);
  assert.match(parseArgs(["doctor", "x"]).errors.join(), /unexpected argument "x"/);
  assert.equal(parseArgs(["check", URL_REF, "--no-verify"]).verify, false);
});

test("a non-https artifact is refused locally, before any request is made", async () => {
  const n = network([]);
  const err = sink();
  const res = await runCheck(CONFIG, "ftp://example.com/x.js", io(n.impl, sink(), err));
  assert.equal(res.code, EXIT.USAGE);
  assert.equal(n.posts.length + n.gets.length, 0, "must not spend a round trip on an input we can already reject");
  assert.match(err.text(), /https URL/);
});

// --- routes ---

// Someone who has never heard of Kenwea types one line and gets a verdict. With
// no key it asks the keyless notary and creates nothing on their behalf.
test("with no key it asks the keyless notary and creates nothing", async () => {
  const { res, n, out } = await check(record({}), {}, CONFIG);
  assert.equal(res.code, EXIT.OK);
  assert.equal(res.verdict, "approved");
  assert.equal(n.posts.length, 2, "initialize and the check, nothing else");
  assert.equal(n.posts[1].params.name, "kenwea.notary.check");
  assert.ok(n.postUrls.every((u) => u === "https://mcp.example/notary/v1"), `went to ${n.postUrls.join(", ")}`);
  assert.ok(!n.posts.some((c) => c.params && c.params.name === "kenwea.onboarding.registerSelf"));
  assert.doesNotMatch(out, /kw_/);
});

test("an existing key keeps the keyed route and its own quota", async () => {
  const { n } = await check(record({}));
  assert.equal(n.posts[1].params.name, "kenwea.sandbox.check");
  assert.ok(n.postUrls.every((u) => u === CONFIG.url));
});

test("a notary refusal is no answer, with the way around a rate limit", async () => {
  const n = network([
    INIT,
    { jsonrpc: "2.0", id: 1, result: { isError: true, content: [{ type: "text", text: JSON.stringify({ error: "rate_limited", detail: "resets in 60 seconds" }) }] } },
  ]);
  const err = sink();
  const res = await runCheck(CONFIG, URL_REF, io(n.impl, sink(), err));
  assert.equal(res.code, EXIT.NO_ANSWER);
  assert.match(err.text(), /rate_limited: resets in 60 seconds/);
  assert.match(err.text(), /KENWEA_API_KEY/);
});

// --- an honest non-answer ---

test("an unfetchable artifact reports why, claims no verdict, and is not a failure without a gate", async () => {
  const { res, out } = await check({ checked: false, reason: "scheme_not_fetchable", note: "nothing was executed" });
  assert.equal(res.code, EXIT.OK);
  assert.equal(res.verdict, null);
  assert.match(out, /NOT CHECKED/);
  assert.doesNotMatch(out, /APPROVED/);
});

// A gate that asked a question and got no answer must not pass the build.
test("with a gate, an honest non-answer fails closed", async () => {
  const { res, err } = await check({ checked: false, reason: "http_404" }, { failOn: "rejected" });
  assert.equal(res.code, EXIT.NO_ANSWER);
  assert.match(err, /nothing was checked/);
});

// --- verification, here and not by asking the server ---

test("a signed record is verified here against the key it names", async () => {
  const { res, out } = await check(record({ verdict: "rejected", reasonCode: "exited_nonzero", verdictReason: "the artifact ran and exited non-zero" }, { unsigned: { output: "Error: Cannot find module './src/common'", executable: "node", exitCode: 1 } }));
  assert.equal(res.code, EXIT.OK, "reporting a verdict is success when nothing is gated");
  assert.equal(res.verdict, "rejected");
  assert.match(out, /REJECTED \(exited_nonzero\)/);
  assert.match(out, /Cannot find module/, "a verdict without the reason behind it is not actionable");
  assert.match(out, new RegExp(`verified    here, against the published key ${ACTIVE.entry.keyId} \\(active\\)`));
});

test("records that must not be believed exit 4, each with its reason", async () => {
  const good = record({});
  const tampered = structuredClone(good);
  tampered.signedAttestation.payload = tampered.signedAttestation.payload.replace('"approved"', '"rejected"');
  tampered.verdict = "rejected";
  const unsigned = { ...good, signedAttestation: undefined };
  /** @type {Record<string, [any, RegExp]>} */
  const cases = {
    "altered after signing": [tampered, /does not match the payload/],
    "signed with a revoked key": [record({}, { key: REVOKED }), /revoked \(the seed was kept/],
    "an unpublished key": [record({}, { key: makeKey("active") }), /not in the published key list/],
    "no signature": [unsigned, /no signature/],
    "about another address": [record({ artifactRef: "https://example.com/other.js" }), /is about https:\/\/example.com\/other.js/],
    "unsigned verdict disagrees": [record({ verdict: "rejected" }, { unsigned: { verdict: "approved" } }), /unsigned verdict differs/],
    "another issuer": [record({ issuer: "example.org" }), /issuer/],
  };
  for (const [name, [result, why]] of Object.entries(cases)) {
    const { res, err } = await check(result, { failOn: "rejected" });
    assert.equal(res.code, EXIT.UNVERIFIED, name);
    assert.match(err, why, name);
  }
});

test("a record from before keys were named is tried against every published key", async () => {
  const { res } = await check(record({}, { format1: true }));
  assert.equal(res.code, EXIT.OK);
  const { res: old, err } = await check(record({}, { format1: true, key: REVOKED }));
  assert.equal(old.code, EXIT.UNVERIFIED);
  assert.match(err, /revoked/);
});

test("--no-verify is for a server without Kenwea's key, and gates still read the record", async () => {
  const { res } = await check({ checked: true, verdict: "approved", ran: true, exitCode: 0 }, { verify: false });
  assert.equal(res.code, EXIT.OK);
  const { res: gated } = await check({ checked: true, verdict: "rejected" }, { verify: false, failOn: "rejected" });
  assert.equal(gated.code, EXIT.GATE);
});

// --- gates read the signed facts ---

test("verdict gates: rejected fails on rejected; manual_review also on manual_review", async () => {
  for (const [verdict, gate, code] of [
    ["rejected", "rejected", EXIT.GATE],
    ["manual_review", "rejected", EXIT.OK],
    ["approved", "rejected", EXIT.OK],
    ["manual_review", "manual_review", EXIT.GATE],
    ["rejected", "manual_review", EXIT.GATE],
    ["notarized", "manual_review", EXIT.OK],
    ["approved", undefined, EXIT.OK],
    ["rejected", undefined, EXIT.OK],
  ]) {
    const { res } = await check(record({ verdict }), { failOn: gate });
    assert.equal(res.code, code, `${verdict} under --fail-on ${gate}`);
  }
});

test("a verdict this client does not know fails a verdict gate closed", () => {
  assert.match(gateRefusals(["rejected"], { verdict: "approved_with_caveats" }).join(), /not one this client knows/);
});

test("install-scripts fails when anything runs at install, and when the record does not say", () => {
  assert.deepEqual(gateRefusals(["install-scripts"], { verdict: "manual_review", installSteps: [] }), []);
  assert.match(gateRefusals(["install-scripts"], { verdict: "approved", installSteps: ["postinstall"] }).join(), /runs 1 install step \(postinstall\)/);
  assert.match(gateRefusals(["install-scripts"], { verdict: "approved" }).join(), /does not say what runs at install/);
});

test("network fails on any attempt, passes when nothing ran, and cannot pass on a cut trace", () => {
  const quiet = { network: [], dns: [], programs: ["node"], missing: [], truncated: false };
  assert.deepEqual(gateRefusals(["network"], { installSteps: ["postinstall"], observed: quiet }), []);
  assert.deepEqual(gateRefusals(["network"], { installSteps: [] }), [], "nothing ran, so nothing reached");
  assert.match(gateRefusals(["network"], { installSteps: ["postinstall"], observed: { ...quiet, dns: ["scarf.sh"] } }).join(), /scarf\.sh/);
  assert.match(gateRefusals(["network"], { installSteps: ["postinstall"], observed: { ...quiet, network: ["93.184.215.14:443"] } }).join(), /93\.184/);
  assert.match(gateRefusals(["network"], { installSteps: ["postinstall"], observed: { ...quiet, truncated: true } }).join(), /cannot say/);
  assert.match(gateRefusals(["network"], { installSteps: ["postinstall"] }).join(), /cannot say/);
  // esbuild on 2026-10-08: stopped at our step limit having reached nothing yet.
  assert.match(gateRefusals(["network"], { reasonCode: "step_timed_out", installSteps: ["postinstall"], observed: { ...quiet, programs: ["node", "npm"] } }).join(), /did not finish/);
});

test("several gates are all applied, from the signed facts", async () => {
  const { res, err } = await check(
    record({ verdict: "manual_review", reasonCode: "ran_tried_network", installSteps: ["postinstall"], observed: { network: [], dns: ["scarf.sh"], programs: ["node"], missing: [], truncated: false } }),
    { failOn: "install-scripts,network" },
  );
  assert.equal(res.code, EXIT.GATE);
  assert.match(err, /install step/);
  assert.match(err, /scarf\.sh/);
});

// --- an npm package: resolved, checked, and tied to the bytes npm installs ---

const TARBALL_URL = "https://registry.npmjs.org/express/-/express-4.18.2.tgz";
const BYTES = Buffer.from("the tarball npm would install");
const SHA256 = createHash("sha256").update(BYTES).digest("hex");
const MANIFEST = {
  name: "express",
  version: "4.18.2",
  dist: { tarball: TARBALL_URL, integrity: "sha512-" + createHash("sha512").update(BYTES).digest("base64") },
};

/** @param {any} result @param {any} [web] @param {any} [opts] */
async function checkPackage(result, web = {}, opts = {}) {
  const n = network([INIT, toolResult(result)], { manifest: MANIFEST, tarball: BYTES, ...web });
  const out = sink();
  const err = sink();
  const res = await runCheck(KEYED, "express@4.18.2", io(n.impl, out, err), opts);
  return { res, n, out: out.text(), err: err.text() };
}

test("a package name is resolved, checked, and the record tied to the tarball npm installs", async () => {
  const { res, n, out } = await checkPackage(record({ artifactRef: TARBALL_URL, contentSha256: SHA256, installSteps: [] }));
  assert.equal(res.code, EXIT.OK);
  assert.ok(n.gets.some((u) => /registry\.npmjs\.org\/express\/4\.18\.2$/.test(u)), "asks the registry");
  assert.equal(n.posts[1].params.arguments.artifactRef, TARBALL_URL, "checks the resolved tarball, not the spec");
  assert.ok(n.gets.includes(TARBALL_URL), "downloads the tarball to compare");
  assert.match(out, /Resolved express@4\.18\.2/);
  assert.match(out, /at install  nothing runs/);
});

test("a record about other bytes than npm's is refused", async () => {
  const other = await checkPackage(record({ artifactRef: TARBALL_URL, contentSha256: "cd".repeat(32) }));
  assert.equal(other.res.code, EXIT.UNVERIFIED);
  assert.match(other.err, /not about the bytes npm installs/);
  const badIntegrity = await checkPackage(record({ artifactRef: TARBALL_URL, contentSha256: SHA256 }), { manifest: { ...MANIFEST, dist: { ...MANIFEST.dist, integrity: "sha512-AAAA" } } });
  assert.equal(badIntegrity.res.code, EXIT.UNVERIFIED);
  assert.match(badIntegrity.err, /integrity/);
});

test("a package the registry does not know is no answer", async () => {
  const { res, err } = await checkPackage(record({}), { manifest: null });
  assert.equal(res.code, EXIT.NO_ANSWER);
  assert.match(err, /could not resolve/);
});

// --- output ---

test("--json prints only the result object on stdout, notices on stderr", async () => {
  const { res, out, err } = await checkPackage(record({ artifactRef: TARBALL_URL, contentSha256: SHA256 }), {}, { json: true });
  assert.equal(res.code, EXIT.OK);
  assert.equal(JSON.parse(out).contentSha256, SHA256);
  assert.match(err, /Resolved express/);
  assert.doesNotMatch(out, /Resolved/);
});

test("--json still honours --fail-on for the exit code", async () => {
  const { res, out } = await check(record({ verdict: "rejected" }), { json: true, failOn: "rejected" });
  assert.equal(res.code, EXIT.GATE);
  assert.equal(JSON.parse(out).verdict, "rejected", "and the JSON is still emitted");
});

// A package's own install output reaches a CI log through this command. It must
// not be able to speak to the runner or the terminal.
test("output from the package cannot issue workflow commands or escape sequences", async () => {
  const hostile = "fine\n::set-output name=verdict::approved\n  ::add-mask::x\n##[error]fake\n\u001b[2Jcleared\u0007";
  const { out } = await check(record({}, { unsigned: { output: hostile } }));
  assert.doesNotMatch(out, /^\s*::/m);
  assert.doesNotMatch(out, /##\[/);
  assert.doesNotMatch(out, /\u001b|\u0007/);
  assert.match(out, /: :set-output/);
  assert.equal(sanitize("a\u001b]0;title\u0007b"), "ab");
});
