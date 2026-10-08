// Kenwea MCP bridge — `kenwea check <pkg|url>`.
//
// The one thing this package can do for someone who has never heard of Kenwea and
// is not going to sell anything: fetch an artifact, run it in a confined sandbox,
// and hand back a verdict they did not produce themselves.
//
// WITH NO KEY IT ASKS THE KEYLESS NOTARY. Until 0.2.7 this command minted an
// anonymous key through `registerSelf` on every first run, because the check
// needed one. Since 2026-09-29 the notary has its own endpoint (/notary/v1) that
// needs no key and is bounded per network address instead, so a first run creates
// nothing on anyone's behalf. A caller who sets KENWEA_API_KEY keeps the keyed
// route and that key's own quota.
//
// SINCE 0.3.0 IT CHECKS WHAT IT IS TOLD. Until then the CLI printed whatever the
// server said, and a gate read the unsigned top-level verdict. Now, before any
// verdict is shown or gated on:
//   - the record's Ed25519 signature is verified here, against the key the
//     payload names in Kenwea's published key list, and a revoked key is refused;
//   - the signed facts must be about what was asked: the same address, the same
//     sha256 and the same verdict as the unsigned fields beside them;
//   - for an npm package, the tarball is downloaded here too, and its sha512 must
//     match the registry's dist.integrity and its sha256 the record's, so the
//     verdict is about the bytes `npm install` will use.
// Gates read the signed facts, never the unsigned copy. A gate that cannot get an
// answer, or gets one it cannot verify, fails closed.

import { createHash, createPublicKey, verify as verifySignature } from "node:crypto";

import { buildHeaders } from "./protocol.js";
import { redact } from "./config.js";

/**
 * @typedef {import("./config.js").BridgeConfig} BridgeConfig
 */

export const KEYS_URL = "https://www.kenwea.com/.well-known/kenwea-attestation-keys.json";

/**
 * Exit codes, distinct so a pipeline can tell "the gate refused" from "we got no
 * answer" from "the answer did not verify".
 */
export const EXIT = Object.freeze({ OK: 0, GATE: 1, USAGE: 2, NO_ANSWER: 3, UNVERIFIED: 4 });

/** What --fail-on accepts. Several can be given, comma separated. */
export const GATES = Object.freeze(["manual_review", "rejected", "install-scripts", "network"]);

const KNOWN_VERDICTS = new Set(["approved", "notarized", "manual_review", "rejected"]);
const UNFINISHED = new Set(["step_timed_out", "run_incomplete", "trace_unavailable", "runner_busy", "not_run"]);
// The notary's own chain is a 20 s fetch, a wait for a slot and a 55 s run inside
// the MCP server's 90 s; this is a little over that.
const CHECK_TIMEOUT_MS = 120_000;
const FETCH_TIMEOUT_MS = 30_000;
const MAX_TARBALL_BYTES = 10 * 1024 * 1024;
const SPKI_ED25519_PREFIX = Buffer.from("302a300506032b6570032100", "hex");

/**
 * @param {BridgeConfig} config
 * @param {string} artifactRef
 * @param {{fetchImpl?: typeof fetch, stdout?: NodeJS.WritableStream, stderr?: NodeJS.WritableStream}} [io]
 * @param {{failOn?: string|string[], json?: boolean, verify?: boolean, keysUrl?: string}} [opts]
 * @returns {Promise<{code: number, ok: boolean, verdict: string|null, result: any}>}
 */
export async function runCheck(config, artifactRef, io = {}, opts = {}) {
  const doFetch = io.fetchImpl ?? fetch;
  const out = io.stdout ?? process.stdout;
  const err = io.stderr ?? process.stderr;
  // In --json mode stdout must be nothing but the result object, so a script or a
  // GitHub Action can parse it. Progress notices (resolved-to) go to
  // stderr instead, where they still show in a CI log without corrupting the JSON.
  const notice = opts.json ? err : out;
  const gates = normaliseGates(opts.failOn);
  const gated = gates.length > 0;
  const verifying = opts.verify !== false;
  /** @param {number} code @param {string|null} verdict @param {any} result */
  const done = (code, verdict, result) => ({ code, ok: code === EXIT.OK, verdict, result });

  if (!artifactRef) {
    err.write("[kenwea-mcp] check needs a package or an https URL, e.g. `kenwea check express@4.18.2` or `kenwea check https://example.com/tool.js`\n");
    return done(EXIT.USAGE, null, null);
  }

  // A developer and a CI job think in package names, not registry tarball URLs. If
  // the argument is not already an https URL, treat it as an npm spec (name,
  // name@version, @scope/name@version), resolve it against the registry, and check
  // the exact tarball `npm install` would download -- printing what we resolved so
  // the bytes under the verdict are never a mystery.
  let ref = artifactRef.trim();
  /** @type {{tarball: string, resolved: string, integrity: string, shasum: string}|null} */
  let pkg = null;
  if (!/^https:\/\//i.test(ref)) {
    // A scheme that is not https is a malformed URL, not a package name -- reject it
    // locally rather than spending a registry round trip trying to resolve `ftp://…`
    // as a package. A real npm spec never contains `://`.
    if (ref.includes("://")) {
      err.write(`[kenwea-mcp] check needs an https URL or an npm package name, not "${sanitize(ref)}"\n`);
      return done(EXIT.USAGE, null, null);
    }
    const resolved = await resolvePackageTarball(doFetch, ref);
    if (!resolved.ok) {
      err.write(`[kenwea-mcp] could not resolve "${sanitize(ref)}" as an npm package: ${sanitize(resolved.error)}\n`);
      return done(EXIT.NO_ANSWER, null, null);
    }
    notice.write(`Resolved ${sanitize(ref)} → ${sanitize(resolved.resolved)}\n  ${sanitize(resolved.tarball)}\n\n`);
    pkg = resolved;
    ref = resolved.tarball;
  }

  /** @type {{sessionId: string|null}} */
  const session = { sessionId: null };
  const keyless = !config.apiKey;
  const active = keyless ? { ...config, url: notaryUrl(config.url) } : config;

  await rpc(doFetch, active, session, {
    jsonrpc: "2.0",
    id: "check-initialize",
    method: "initialize",
    params: {
      protocolVersion: config.protocolVersion,
      capabilities: {},
      clientInfo: { name: "@kenwea/mcp", version: "check" },
    },
  });

  const res = await rpc(
    doFetch,
    active,
    session,
    {
      jsonrpc: "2.0",
      id: "check-sandbox",
      method: "tools/call",
      params: { name: keyless ? "kenwea.notary.check" : "kenwea.sandbox.check", arguments: { artifactRef: ref } },
    },
    CHECK_TIMEOUT_MS,
  );
  if (!res.ok) {
    err.write(`[kenwea-mcp] check failed: ${sanitize(detail(res))}\n`);
    return done(EXIT.NO_ANSWER, null, null);
  }
  // The notary reports a refusal (rate limit, unreadable argument) as a tool
  // result marked isError rather than as a protocol error.
  if (res.body && res.body.result && res.body.result.isError) {
    const refusal = extractToolResult(res.body) || {};
    const hint = refusal.error === "rate_limited" ? " (set KENWEA_API_KEY to use a key's own quota)" : "";
    err.write(`[kenwea-mcp] check refused: ${sanitize(refusal.error ?? "error")}: ${sanitize(refusal.detail ?? "")}${hint}\n`);
    return done(EXIT.NO_ANSWER, null, null);
  }

  const result = extractToolResult(res.body);
  if (!result || typeof result !== "object") {
    err.write("[kenwea-mcp] the server returned no readable result\n");
    return done(EXIT.NO_ANSWER, null, null);
  }

  // An honest non-answer: the server could not read the artifact. Reporting it is
  // a success when nothing is gated on it; a gate that asked a question and got no
  // answer fails closed, because a green build that checked nothing is the one
  // outcome a gate exists to prevent.
  if (result.checked === false) {
    out.write(opts.json ? JSON.stringify(result) + "\n" : render(result, ref, null));
    if (gated) {
      err.write(`[kenwea-mcp] gate: nothing was checked (${sanitize(result.reason ?? "no reason given")}), so --fail-on ${gates.join(",")} cannot pass\n`);
      return done(EXIT.NO_ANSWER, null, result);
    }
    return done(EXIT.OK, null, result);
  }

  /** @type {any} */
  let facts = result;
  /** @type {{keyId: string, status: string}|null} */
  let signer = null;
  if (verifying) {
    const checked = await verifyRecord(doFetch, result, ref, opts.keysUrl ?? KEYS_URL);
    if (!checked.ok) {
      out.write(opts.json ? JSON.stringify(result) + "\n" : render(result, ref, null));
      err.write(`[kenwea-mcp] the record did not verify: ${sanitize(checked.error)}\n`);
      return done(EXIT.UNVERIFIED, null, result);
    }
    facts = checked.facts;
    signer = checked.key;
    if (pkg) {
      const match = await matchTarball(doFetch, pkg, String(facts.contentSha256 ?? ""));
      if (!match.ok) {
        out.write(opts.json ? JSON.stringify(result) + "\n" : render(result, ref, signer));
        err.write(`[kenwea-mcp] the record is not about the bytes npm installs: ${sanitize(match.error)}\n`);
        return done(EXIT.UNVERIFIED, null, result);
      }
    }
  }

  out.write(opts.json ? JSON.stringify(result) + "\n" : render(result, ref, signer));

  // CI gate. Without --fail-on the command exits 0 whenever it got a verified
  // answer: reporting a verdict is a success even when the verdict is "rejected",
  // and exiting non-zero on a clean report would train pipelines to treat "we read
  // it" as "it is bad". With --fail-on, the signed facts decide.
  const verdict = typeof facts.verdict === "string" ? facts.verdict : null;
  if (gated) {
    const refusals = gateRefusals(gates, facts);
    if (refusals.length) {
      for (const why of refusals) err.write(`[kenwea-mcp] gate: ${why}\n`);
      return done(EXIT.GATE, verdict, result);
    }
  }
  return done(EXIT.OK, verdict, result);
}

/**
 * @param {string|string[]|undefined} failOn
 * @returns {string[]}
 */
export function normaliseGates(failOn) {
  if (!failOn) return [];
  const list = Array.isArray(failOn) ? failOn : String(failOn).split(",");
  return [...new Set(list.map((g) => g.trim()).filter(Boolean))];
}

/**
 * Why the gates refuse these signed facts; empty when they pass. Anything the
 * record does not say is a refusal: a gate cannot pass on silence.
 * @param {string[]} gates
 * @param {any} facts
 * @returns {string[]}
 */
export function gateRefusals(gates, facts) {
  /** @type {string[]} */
  const refusals = [];
  const verdict = facts && typeof facts.verdict === "string" ? facts.verdict : "";
  const steps = facts && Array.isArray(facts.installSteps) ? facts.installSteps : null;
  const observed = facts && facts.observed && typeof facts.observed === "object" ? facts.observed : null;
  for (const gate of gates) {
    switch (gate) {
      case "rejected":
      case "manual_review":
        if (!KNOWN_VERDICTS.has(verdict)) {
          refusals.push(`the verdict "${sanitize(verdict)}" is not one this client knows, so --fail-on ${gate} cannot pass`);
        } else if (verdict === "rejected" || (gate === "manual_review" && verdict === "manual_review")) {
          refusals.push(`verdict "${verdict}" is at or past --fail-on ${gate}`);
        }
        break;
      case "install-scripts":
        if (!steps) refusals.push("the record does not say what runs at install, so --fail-on install-scripts cannot pass");
        else if (steps.length) refusals.push(`it runs ${steps.length} install step${steps.length === 1 ? "" : "s"} (${steps.map(sanitize).join(", ")}) and --fail-on install-scripts is set`);
        break;
      case "network": {
        if (steps && steps.length === 0) break; // nothing ran, so nothing reached
        // A run that did not finish was watched only until it stopped.
        if (UNFINISHED.has(facts.reasonCode)) {
          refusals.push(`the install steps did not finish (${sanitize(facts.reasonCode)}), so the record cannot say what they would have reached and --fail-on network cannot pass`);
          break;
        }
        if (!observed || observed.truncated) {
          refusals.push("the record cannot say what was attempted, so --fail-on network cannot pass");
          break;
        }
        const reached = [...(Array.isArray(observed.dns) ? observed.dns : []), ...(Array.isArray(observed.network) ? observed.network : [])];
        if (reached.length) refusals.push(`it tried to reach the network (${reached.map(sanitize).join(", ")}) and --fail-on network is set`);
        break;
      }
      default:
        refusals.push(`unknown gate "${sanitize(gate)}"`);
    }
  }
  return refusals;
}

/**
 * Verify a record here, without asking Kenwea whether Kenwea's signature is good.
 * @param {typeof fetch} doFetch
 * @param {any} result
 * @param {string} ref the address that was asked about
 * @param {string} keysUrl
 * @returns {Promise<{ok: true, facts: any, key: {keyId: string, status: string}} | {ok: false, error: string}>}
 */
export async function verifyRecord(doFetch, result, ref, keysUrl) {
  const signed = result && result.signedAttestation;
  if (!signed || typeof signed.payload !== "string" || typeof signed.signature !== "string") {
    return { ok: false, error: "the record carries no signature" };
  }
  /** @type {any} */
  let facts;
  try {
    facts = JSON.parse(signed.payload);
  } catch {
    return { ok: false, error: "the signed payload is not JSON" };
  }
  if (!facts || facts.issuer !== "kenwea.com") return { ok: false, error: "the signed payload does not name kenwea.com as its issuer" };

  const listed = await fetchKeys(doFetch, keysUrl);
  if (!listed.ok) return { ok: false, error: `could not read the published keys at ${keysUrl}: ${listed.error}` };
  const named = typeof facts.keyId === "string" ? facts.keyId : "";
  // A format 2 record names its key; an older one is tried against every key.
  const candidates = named ? listed.keys.filter((k) => k.keyId === named) : listed.keys;
  if (named && !candidates.length) return { ok: false, error: `the record names key ${named}, which is not in the published key list` };
  const sig = Buffer.from(signed.signature, "base64");
  const signer = candidates.find((k) => {
    try {
      const key = createPublicKey({ key: Buffer.concat([SPKI_ED25519_PREFIX, Buffer.from(k.publicKeyBase64, "base64")]), format: "der", type: "spki" });
      return verifySignature(null, Buffer.from(signed.payload, "utf8"), key, sig);
    } catch {
      return false;
    }
  });
  if (!signer) return { ok: false, error: "the signature does not match the payload under any published key: the record was altered, re-serialised or signed by someone else" };
  if (signer.status === "revoked") {
    return { ok: false, error: `it was signed with key ${signer.keyId}, which Kenwea has revoked${signer.reason ? ` (${signer.reason})` : ""}, so it proves nothing` };
  }
  if (facts.artifactRef !== ref) return { ok: false, error: `the signed record is about ${facts.artifactRef}, not ${ref}` };
  if (result.contentSha256 !== undefined && result.contentSha256 !== facts.contentSha256) {
    return { ok: false, error: "the unsigned contentSha256 differs from the signed one" };
  }
  if (result.verdict !== undefined && result.verdict !== facts.verdict) {
    return { ok: false, error: "the unsigned verdict differs from the signed one" };
  }
  return { ok: true, facts, key: { keyId: signer.keyId, status: signer.status } };
}

/**
 * Kenwea's published keys, each kept only if its keyId is the one derived from
 * the key itself, so an entry cannot claim another key's id.
 * @param {typeof fetch} doFetch
 * @param {string} keysUrl
 * @returns {Promise<{ok: true, keys: Array<{keyId: string, publicKeyBase64: string, status: string, reason?: string}>} | {ok: false, error: string}>}
 */
async function fetchKeys(doFetch, keysUrl) {
  try {
    const res = await doFetch(keysUrl, { headers: { accept: "application/json" }, signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
    if (!res.ok) return { ok: false, error: `HTTP ${res.status}` };
    const doc = JSON.parse(await res.text());
    const keys = (Array.isArray(doc && doc.keys) ? doc.keys : []).filter(
      (/** @type {any} */ k) =>
        k &&
        k.algorithm === "ed25519" &&
        typeof k.publicKeyBase64 === "string" &&
        Buffer.from(k.publicKeyBase64, "base64").length === 32 &&
        createHash("sha256").update(k.publicKeyBase64).digest("hex").slice(0, 16) === k.keyId,
    );
    if (!keys.length) return { ok: false, error: "it holds no usable key" };
    return { ok: true, keys };
  } catch (e) {
    return { ok: false, error: String((e && /** @type {any} */ (e).message) || e) };
  }
}

/**
 * Download the tarball npm would install and confirm the record is about it: the
 * registry's integrity hash must match these bytes, and so must the record's
 * sha256.
 * @param {typeof fetch} doFetch
 * @param {{tarball: string, integrity: string, shasum: string}} pkg
 * @param {string} recordSha256
 * @returns {Promise<{ok: true} | {ok: false, error: string}>}
 */
async function matchTarball(doFetch, pkg, recordSha256) {
  /** @type {Buffer} */
  let bytes;
  try {
    const res = await doFetch(pkg.tarball, { signal: AbortSignal.timeout(FETCH_TIMEOUT_MS * 2) });
    if (!res.ok) return { ok: false, error: `the tarball returned HTTP ${res.status}` };
    bytes = Buffer.from(await res.arrayBuffer());
  } catch (e) {
    return { ok: false, error: `could not download the tarball: ${String((e && /** @type {any} */ (e).message) || e)}` };
  }
  if (bytes.length > MAX_TARBALL_BYTES) return { ok: false, error: "the tarball is larger than the notary reads" };
  const sriOk = pkg.integrity
    ? pkg.integrity
        .split(/\s+/)
        .filter((s) => s.startsWith("sha512-"))
        .some((s) => s.slice(7) === createHash("sha512").update(bytes).digest("base64"))
    : !!pkg.shasum && pkg.shasum === createHash("sha1").update(bytes).digest("hex");
  if (!sriOk) return { ok: false, error: "the downloaded tarball does not match the registry's integrity hash" };
  const sha256 = createHash("sha256").update(bytes).digest("hex");
  if (sha256 !== recordSha256.toLowerCase()) {
    return { ok: false, error: `the registry's tarball has sha256 ${sha256}, the record ${recordSha256 || "none"}` };
  }
  return { ok: true };
}

/**
 * Resolve an npm package spec to the exact tarball URL `npm install` would fetch.
 * Accepts `name`, `name@version`, `@scope/name`, `@scope/name@version`; an absent
 * version means the registry's `latest` dist-tag.
 * @param {typeof fetch} doFetch
 * @param {string} spec
 * @returns {Promise<{ok: true, tarball: string, resolved: string, integrity: string, shasum: string} | {ok: false, error: string}>}
 */
async function resolvePackageTarball(doFetch, spec) {
  const s = spec.trim();
  // The scope's own leading "@" is not the version separator; look past it.
  const sep = s.indexOf("@", s.startsWith("@") ? 1 : 0);
  const name = sep === -1 ? s : s.slice(0, sep);
  const version = sep === -1 ? "latest" : s.slice(sep + 1);
  if (!name) return { ok: false, error: "empty package name" };

  // The per-version manifest, not the full packument: it carries dist.tarball
  // directly and is a fraction of the bytes. Scoped names keep their slash here.
  const url = `https://registry.npmjs.org/${name}/${encodeURIComponent(version)}`;
  try {
    const res = await doFetch(url, { headers: { accept: "application/json" }, signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
    if (!res.ok) {
      return { ok: false, error: `registry returned HTTP ${res.status} for ${name}@${version}` };
    }
    const body = /** @type {any} */ (JSON.parse(await res.text()));
    const tarball = body && body.dist && body.dist.tarball;
    if (typeof tarball !== "string" || !/^https:\/\//i.test(tarball)) {
      return { ok: false, error: `no tarball URL in the registry manifest for ${name}@${version}` };
    }
    return {
      ok: true,
      tarball,
      resolved: `${body.name}@${body.version}`,
      integrity: typeof body.dist.integrity === "string" ? body.dist.integrity : "",
      shasum: typeof body.dist.shasum === "string" ? body.dist.shasum : "",
    };
  } catch (e) {
    return { ok: false, error: String((e && /** @type {any} */ (e).message) || e) };
  }
}

/**
 * Text from the server, much of it a package's own install output, made safe to
 * print into a terminal or a CI log: no escape sequences or other control
 * characters, and nothing a GitHub runner would read as a workflow command
 * ("::set-output", "::add-mask::", "##[...]") at the start of a line.
 * @param {unknown} text
 * @returns {string}
 */
export function sanitize(text) {
  return String(text ?? "")
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?/g, "")
    .replace(/[\x00-\x08\x0b-\x1f\x7f\x9b]/g, "")
    .replace(/^(\s*)::/gm, "$1: :")
    .replace(/##\[/g, "## [");
}

/**
 * @param {any} result
 * @param {string} artifactRef
 * @param {{keyId: string, status: string}|null} signer the key the record verified under, if it was verified
 * @returns {string}
 */
function render(result, artifactRef, signer) {
  const lines = [`Kenwea notary — ${sanitize(artifactRef)}`, ""];
  if (result.checked === false) {
    lines.push(`  NOT CHECKED  ${sanitize(result.reason ?? "unknown")}`);
    if (result.note) lines.push(`  ${sanitize(result.note)}`);
    lines.push("");
    return lines.join("\n");
  }
  lines.push(`  verdict     ${sanitize(String(result.verdict ?? "unknown").toUpperCase())}${result.reasonCode ? ` (${sanitize(result.reasonCode)})` : ""}`);
  if (result.verdictReason) lines.push(`  why         ${sanitize(result.verdictReason)}`);
  lines.push(`  sha256      ${sanitize(result.contentSha256 ?? "-")}`);
  lines.push(`  size        ${sanitize(result.contentSizeBytes ?? "-")} bytes`);
  if (Array.isArray(result.installSteps)) {
    lines.push(`  at install  ${result.installSteps.length ? result.installSteps.map(sanitize).join(", ") : "nothing runs"}`);
  }
  const observed = result.observed;
  if (observed && typeof observed === "object") {
    const reached = [...(observed.dns || []), ...(observed.network || [])].map(sanitize);
    lines.push(`  reached     ${reached.length ? reached.join(", ") : "nothing"}${observed.truncated ? " (trace cut, may be incomplete)" : ""}`);
    if (Array.isArray(observed.programs) && observed.programs.length) lines.push(`  started     ${observed.programs.map(sanitize).join(", ")}`);
    if (Array.isArray(observed.missing) && observed.missing.length) lines.push(`  not found   ${observed.missing.map(sanitize).join(", ")}`);
  }
  lines.push(`  executed    ${result.ran ? `yes (${sanitize(result.executable)})` : `no${result.notRunReason ? ` — ${sanitize(result.notRunReason)}` : ""}`}`);
  if (result.ran) lines.push(`  exit code   ${sanitize(result.exitCode)}`);
  for (const [label, key] of [["secrets", "secretHits"], ["danger", "dangerHits"]]) {
    const hits = result[key];
    if (Array.isArray(hits) && hits.length) lines.push(`  ${label}     ${hits.map(sanitize).join(", ")}`);
  }
  if (result.ran && result.output) {
    lines.push("", "  output:");
    for (const line of sanitize(result.output).split("\n").slice(0, 20)) lines.push(`    ${line}`);
  }
  if (result.attestation) lines.push("", `  ${sanitize(result.attestation)}`);
  if (result.cached) lines.push("", "  cached      the record issued for these bytes earlier; nothing was run again");

  // The signature is the difference between "Kenwea says this is fine" and evidence
  // you can forward, so the CLI surfaces it rather than leaving it in the JSON where
  // only someone already looking would find it.
  const signed = result.signedAttestation;
  if (signed && signed.signature) {
    lines.push("", `  signed      ${sanitize(signed.algorithm)}, key ${sanitize(signed.keyId)}`);
    lines.push(signer ? `  verified    here, against the published key ${signer.keyId} (${signer.status})` : "  verified    no");
    lines.push(`  keys        ${KEYS_URL}`);
    lines.push("");
    lines.push("  The claim is about the sha256 above, not the URL — that address can");
    lines.push("  serve something else tomorrow.");
  }
  lines.push("");
  return lines.join("\n");
}

/**
 * The tool result arrives as MCP content blocks holding a JSON string. Tolerates
 * both that and a plain structured result, because a client that only works
 * against one server's exact response shape is a client that breaks on the next
 * protocol revision.
 * @param {any} body
 * @returns {any}
 */
function extractToolResult(body) {
  const result = body && body.result;
  if (!result) return null;
  const text =
    Array.isArray(result.content) && result.content[0] && typeof result.content[0].text === "string"
      ? result.content[0].text
      : null;
  if (text) {
    try {
      return JSON.parse(text);
    } catch {
      return null;
    }
  }
  return typeof result === "object" ? result : null;
}

/**
 * The keyless notary lives beside the main endpoint on the same host.
 * @param {string} url
 * @returns {string}
 */
function notaryUrl(url) {
  if (/\/mcp\/v1\/?$/.test(url)) return url.replace(/\/mcp\/v1\/?$/, "/notary/v1");
  return new URL("/notary/v1", url).href;
}

/**
 * @param {typeof fetch} doFetch
 * @param {BridgeConfig} config
 * @param {{sessionId: string|null}} session
 * @param {object} message
 * @param {number} [timeoutMs]
 * @returns {Promise<{ok: boolean, status: number, body: any, error: string|null}>}
 */
async function rpc(doFetch, config, session, message, timeoutMs = FETCH_TIMEOUT_MS) {
  try {
    const res = await doFetch(config.url, {
      method: "POST",
      headers: buildHeaders(config, session, null),
      body: JSON.stringify(message),
      signal: AbortSignal.timeout(timeoutMs),
    });
    const issued = res.headers.get("Mcp-Session-Id");
    if (issued) session.sessionId = issued;
    const text = await res.text();
    /** @type {any} */
    let body = null;
    try {
      body = text ? JSON.parse(text) : null;
    } catch {
      body = null;
    }
    return { ok: res.ok && !!body && body.error === undefined, status: res.status, body, error: null };
  } catch (e) {
    return {
      ok: false,
      status: 0,
      body: null,
      error: redact(String((e && /** @type {any} */ (e).message) || e), config),
    };
  }
}

/**
 * @param {{status: number, body: any, error: string|null}} res
 * @returns {string}
 */
function detail(res) {
  if (res.error) return res.error;
  if (res.body && res.body.error) {
    const e = res.body.error;
    return `${e.message || "error"}${e.data && e.data.detail ? `: ${e.data.detail}` : ""} (HTTP ${res.status})`;
  }
  return `HTTP ${res.status}`;
}
