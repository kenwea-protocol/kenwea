import assert from "node:assert/strict";
import { createHash, generateKeyPairSync, sign } from "node:crypto";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { EXIT, KEYS_URL } from "../src/check.js";
import { readLockfile, runCheckTree } from "../src/tree.js";

const CONFIG = { url: "https://mcp.example/mcp/v1", apiKey: "", protocolVersion: "2025-11-25", correlationId: "" };

const { publicKey, privateKey } = generateKeyPairSync("ed25519");
const publicKeyBase64 = publicKey.export({ format: "der", type: "spki" }).subarray(-32).toString("base64");
const keyId = createHash("sha256").update(publicKeyBase64).digest("hex").slice(0, 16);
const KEYS = { keys: [{ keyId, algorithm: "ed25519", publicKeyBase64, status: "active" }] };

const TARBALL = Buffer.from("esbuild tarball bytes");
const SHA256 = createHash("sha256").update(TARBALL).digest("hex");
const INTEGRITY = "sha512-" + createHash("sha512").update(TARBALL).digest("base64");
const REF = "https://registry.npmjs.org/esbuild/-/esbuild-0.25.0.tgz";

const LOCK = JSON.stringify({
  lockfileVersion: 3,
  packages: {
    "": { name: "app" },
    "node_modules/esbuild": { version: "0.25.0", resolved: REF, integrity: INTEGRITY, hasInstallScript: true },
    "node_modules/@acme/private-thing": { version: "1.0.0" },
    "node_modules/lodash": { version: "4.17.21" },
    "node_modules/sharp": { version: "0.33.0", hasInstallScript: true },
  },
});

/** @param {Record<string, any>} facts */
function signedItem(facts) {
  const signedFacts = { issuer: "kenwea.com", format: 2, keyId, checker: "2026-10-15.3", artifactRef: REF, contentSha256: SHA256, ...facts };
  const payload = JSON.stringify(signedFacts);
  return { name: "esbuild", version: "0.25.0", status: "answered", artifactRef: REF, ...signedFacts, summary: "s",
    signedAttestation: { algorithm: "ed25519", keyId, payload, signature: sign(null, Buffer.from(payload), privateKey).toString("base64") } };
}

/** @param {any[]} answers tree results in order */
function network(answers) {
  /** @type {any[]} */
  const sent = [];
  const doFetch = /** @type {any} */ (async (/** @type {string} */ url, /** @type {any} */ init) => {
    const body = (/** @type {string} */ text, status = 200) => ({ ok: status < 300, status, headers: { get: () => null }, text: async () => text, arrayBuffer: async () => TARBALL });
    if (url === KEYS_URL) return body(JSON.stringify(KEYS));
    if (url === REF) return body("");
    sent.push(JSON.parse(init.body));
    const next = answers.shift();
    return body(JSON.stringify({ jsonrpc: "2.0", id: 1, result: { content: [{ type: "text", text: JSON.stringify(next) }] } }));
  });
  return { doFetch, sent };
}

function lockFile() {
  const dir = mkdtempSync(join(tmpdir(), "kwtree-"));
  const file = join(dir, "package-lock.json");
  writeFileSync(file, LOCK);
  return file;
}

const sink = () => {
  let text = "";
  return { stream: /** @type {any} */ ({ write: (/** @type {string} */ s) => { text += s; return true; } }), get text() { return text; } };
};

test("readLockfile keeps every package here and lists only those that run something", () => {
  const lock = readLockfile(LOCK);
  assert.ok(lock.ok);
  if (!lock.ok) return;
  assert.equal(lock.all, 4);
  assert.deepEqual(lock.runs.map((r) => r.name).sort(), ["esbuild", "sharp"]);
  const v1 = readLockfile(JSON.stringify({ lockfileVersion: 1, dependencies: {} }));
  assert.ok(!v1.ok && /npm 7/.test(v1.error));
});

test("check-tree sends only install-step packages, verifies records, and gates on them", async () => {
  const { doFetch, sent } = network([{ packages: [signedItem({ verdict: "approved", reasonCode: "ran_ok", level: "quiet", installSteps: ["postinstall"], observed: { network: [], dns: [], credentialsRead: [] } }), { name: "sharp", version: "0.33.0", status: "queued" }] }]);
  const out = sink(), err = sink();
  const { code } = await runCheckTree(CONFIG, lockFile(), { fetchImpl: doFetch, stdout: out.stream, stderr: err.stream }, {});
  assert.equal(code, EXIT.OK, err.text);
  const asked = sent[0].params.arguments.packages.map((/** @type {any} */ p) => p.name).sort();
  assert.deepEqual(asked, ["esbuild", "sharp"], "lodash and the private package never leave the machine");
  assert.match(out.text, /QUIET\s+esbuild@0\.25\.0/);
  assert.match(out.text, /QUEUED\s+sharp@0\.33\.0/);

  // A gate cannot pass on a queued package.
  const second = network([{ packages: [signedItem({ verdict: "approved", reasonCode: "ran_ok", level: "quiet", installSteps: ["postinstall"], observed: { network: [], dns: [] } }), { name: "sharp", version: "0.33.0", status: "queued" }] }]);
  const gated = await runCheckTree(CONFIG, lockFile(), { fetchImpl: second.doFetch, stdout: sink().stream, stderr: sink().stream }, { failOn: ["network"] });
  assert.equal(gated.code, EXIT.NO_ANSWER);
});

test("check-tree fails a gate on a finding and refuses a record that does not verify", async () => {
  const bad = network([{ packages: [signedItem({ verdict: "rejected", reasonCode: "sent_credentials", level: "sends_credentials", installSteps: ["postinstall"], observed: { network: [], dns: [], credentialsSent: ["npm token in data it sent"] } })] }]);
  const err = sink();
  const res = await runCheckTree(CONFIG, lockFile(), { fetchImpl: bad.doFetch, stdout: sink().stream, stderr: err.stream }, { failOn: ["credentials"] });
  assert.equal(res.code, EXIT.GATE);
  assert.match(err.text, /sent npm token/);

  const tampered = signedItem({ verdict: "approved", reasonCode: "ran_ok", level: "quiet", installSteps: ["postinstall"], observed: { network: [], dns: [] } });
  tampered.signedAttestation.payload = tampered.signedAttestation.payload.replace('"quiet"', '"quiet" ');
  const forged = network([{ packages: [tampered] }]);
  const res2 = await runCheckTree(CONFIG, lockFile(), { fetchImpl: forged.doFetch, stdout: sink().stream, stderr: sink().stream }, {});
  assert.equal(res2.code, EXIT.UNVERIFIED);
});

test("--wait asks again until the queued package has a record", async () => {
  const { doFetch, sent } = network([
    { packages: [{ name: "esbuild", version: "0.25.0", status: "queued" }] },
    { packages: [signedItem({ verdict: "approved", reasonCode: "ran_ok", level: "quiet", installSteps: ["postinstall"], observed: { network: [], dns: [] } })] },
  ]);
  const { code } = await runCheckTree(CONFIG, lockFile(), { fetchImpl: doFetch, stdout: sink().stream, stderr: sink().stream, sleep: async () => {} }, { failOn: ["network"], waitSeconds: 600 });
  assert.equal(code, EXIT.OK);
  assert.equal(sent.length, 2);
});

// 0.7.0: the tree is checked through the project's direct dependencies, and a
// package under one comes back covered, answered by that dependency's record.
test("check-tree sends the direct dependencies, and a covered package does not hold a gate", async () => {
  const lock = JSON.stringify({
    lockfileVersion: 3,
    packages: {
      "": { name: "app", dependencies: { esbuild: "^0.25.0" }, devDependencies: { lodash: "^4.17.0" } },
      "node_modules/esbuild": { version: "0.25.0", resolved: REF, integrity: INTEGRITY, hasInstallScript: true },
      "node_modules/@scarf/scarf": { version: "1.4.0", hasInstallScript: true },
      "node_modules/lodash": { version: "4.17.21" },
    },
  });
  const dir = mkdtempSync(join(tmpdir(), "kwtree-"));
  const file = join(dir, "package-lock.json");
  writeFileSync(file, lock);
  const answer = {
    checkedThrough: "direct_dependencies",
    packages: [
      { ...signedItem({ verdict: "approved", reasonCode: "ran_ok", level: "quiet", installSteps: ["postinstall"], observed: { network: [], dns: [] } }), direct: true, covers: "esbuild, @scarf/scarf" },
      { name: "@scarf/scarf", version: "1.4.0", status: "covered", coveredBy: "esbuild@0.25.0" },
    ],
  };
  const { doFetch, sent } = network([answer]);
  const out = sink(), err = sink();
  const { code } = await runCheckTree(CONFIG, file, { fetchImpl: doFetch, stdout: out.stream, stderr: err.stream }, { failOn: ["network"] });
  assert.equal(code, EXIT.OK, err.text);
  const args = sent[0].params.arguments;
  assert.deepEqual(args.directs, [{ name: "esbuild", version: "0.25.0" }, { name: "lodash", version: "4.17.21" }]);
  assert.deepEqual(args.packages.map((/** @type {any} */ p) => p.name).sort(), ["@scarf/scarf", "esbuild"], "only what runs, and the directs, leave the machine");
  assert.match(out.text, /UNDER\s+@scarf\/scarf@1\.4\.0, run inside esbuild@0\.25\.0/);
  assert.match(out.text, /Checked through your direct dependencies/);
});
