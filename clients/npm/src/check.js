// Kenwea MCP bridge — `kenwea check <url>`.
//
// The one thing this package can do for someone who has never heard of Kenwea and
// is not going to sell anything: fetch an artifact, run it in a confined sandbox,
// and hand back a verdict they did not produce themselves.
//
// WHY IT SELF-REGISTERS. `kenwea.sandbox.check` needs a key, and that is not
// negotiable -- free compute for a caller nobody can name is an open relay, not an
// open door. But the key costs nothing and needs no human: `registerSelf` is one
// anonymous call. So the command mints one, says out loud that it did, and gets on
// with it. A tool that demands a signup before it will show you what it does has
// already lost the person it was built for.
//
// It prints the key it minted rather than hiding it, because a caller who wants to
// keep using this should not have to re-register on every invocation, and a
// credential quietly created on someone's behalf and never shown to them is the
// kind of thing this project refuses to do elsewhere.

import { buildHeaders } from "./protocol.js";
import { redact } from "./config.js";

/**
 * @typedef {import("./config.js").BridgeConfig} BridgeConfig
 */

/**
 * @param {BridgeConfig} config
 * @param {string} artifactRef
 * @param {{fetchImpl?: typeof fetch, stdout?: NodeJS.WritableStream, stderr?: NodeJS.WritableStream}} [io]
 * @param {{failOn?: string, json?: boolean}} [opts]
 * @returns {Promise<{ok: boolean, verdict: string|null, result: any}>}
 */
export async function runCheck(config, artifactRef, io = {}, opts = {}) {
  const doFetch = io.fetchImpl ?? fetch;
  const out = io.stdout ?? process.stdout;
  const err = io.stderr ?? process.stderr;
  // In --json mode stdout must be nothing but the result object, so a script or a
  // GitHub Action can parse it. Progress notices (resolved-to, key-minted) go to
  // stderr instead, where they still show in a CI log without corrupting the JSON.
  const notice = opts.json ? err : out;

  if (!artifactRef) {
    err.write("[kenwea-mcp] check needs a package or an https URL, e.g. `kenwea check express@4.18.2` or `kenwea check https://example.com/tool.js`\n");
    return { ok: false, verdict: null, result: null };
  }

  // A developer and a CI job think in package names, not registry tarball URLs. If
  // the argument is not already an https URL, treat it as an npm spec (name,
  // name@version, @scope/name@version), resolve it against the registry, and check
  // the exact tarball `npm install` would download -- printing what we resolved so
  // the bytes under the verdict are never a mystery.
  let ref = artifactRef.trim();
  if (!/^https:\/\//i.test(ref)) {
    // A scheme that is not https is a malformed URL, not a package name -- reject it
    // locally rather than spending a registry round trip trying to resolve `ftp://…`
    // as a package. A real npm spec never contains `://`.
    if (ref.includes("://")) {
      err.write(`[kenwea-mcp] check needs an https URL or an npm package name, not "${ref}"\n`);
      return { ok: false, verdict: null, result: null };
    }
    const resolved = await resolvePackageTarball(doFetch, ref);
    if (!resolved.ok) {
      err.write(`[kenwea-mcp] could not resolve "${ref}" as an npm package: ${resolved.error}\n`);
      return { ok: false, verdict: null, result: null };
    }
    notice.write(`Resolved ${ref} → ${resolved.resolved}\n  ${resolved.tarball}\n\n`);
    ref = resolved.tarball;
  }

  /** @type {{sessionId: string|null}} */
  const session = { sessionId: null };
  let active = config;

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

  if (!active.apiKey) {
    notice.write("No KENWEA_API_KEY set — minting a free anonymous key (no signup, no payment)...\n");
    const reg = await rpc(doFetch, active, session, {
      jsonrpc: "2.0",
      id: "check-register",
      method: "tools/call",
      params: {
        name: "kenwea.onboarding.registerSelf",
        arguments: { agentName: "kenwea-mcp-check", keyLabel: "cli-check" },
      },
    });
    const minted = extractKey(reg.body);
    if (!minted) {
      err.write(`[kenwea-mcp] could not obtain a key: ${detail(reg)}\n`);
      return { ok: false, verdict: null, result: null };
    }
    active = { ...active, apiKey: minted };
    notice.write(`Key minted. Reuse it with KENWEA_API_KEY to skip this step:\n  ${minted}\n\n`);
  }

  const res = await rpc(doFetch, active, session, {
    jsonrpc: "2.0",
    id: "check-sandbox",
    method: "tools/call",
    params: { name: "kenwea.sandbox.check", arguments: { artifactRef: ref } },
  });
  if (!res.ok) {
    err.write(`[kenwea-mcp] check failed: ${detail(res)}\n`);
    return { ok: false, verdict: null, result: null };
  }

  const result = extractToolResult(res.body);
  if (!result) {
    err.write("[kenwea-mcp] the server returned no readable result\n");
    return { ok: false, verdict: null, result: null };
  }

  out.write(opts.json ? JSON.stringify(result) + "\n" : render(result, ref));

  // CI gate. By default the command exits 0 whenever it *ran* -- reporting a
  // verdict is a success even when the verdict is "rejected", and exiting non-zero
  // on a clean report would train pipelines to treat "we read it" as "it is bad".
  // A pipeline that wants to BLOCK on a verdict asks for it explicitly with
  // --fail-on, and only then does the verdict decide the exit code.
  //
  // An honest non-answer (checked=false) never fails the gate: the server declined
  // to guess, and turning "we could not read it" into a red build is the exact
  // dishonesty this tool refuses elsewhere.
  const verdict = result.verdict ?? null;
  if (opts.failOn && result.checked !== false) {
    /** @type {Record<string, number>} */
    const rank = { approved: 0, notarized: 0, manual_review: 1, rejected: 2 };
    const threshold = rank[opts.failOn] ?? 0;
    const got = rank[verdict ?? ""] ?? 0;
    if (got >= threshold) {
      err.write(`[kenwea-mcp] gate: verdict "${verdict}" is at or past --fail-on ${opts.failOn}\n`);
      return { ok: false, verdict, result };
    }
  }
  return { ok: true, verdict, result };
}

/**
 * Resolve an npm package spec to the exact tarball URL `npm install` would fetch.
 * Accepts `name`, `name@version`, `@scope/name`, `@scope/name@version`; an absent
 * version means the registry's `latest` dist-tag.
 * @param {typeof fetch} doFetch
 * @param {string} spec
 * @returns {Promise<{ok: true, tarball: string, resolved: string} | {ok: false, error: string}>}
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
    const res = await doFetch(url, { headers: { accept: "application/json" } });
    if (!res.ok) {
      return { ok: false, error: `registry returned HTTP ${res.status} for ${name}@${version}` };
    }
    const body = /** @type {any} */ (await res.json());
    const tarball = body && body.dist && body.dist.tarball;
    if (typeof tarball !== "string" || !/^https:\/\//i.test(tarball)) {
      return { ok: false, error: `no tarball URL in the registry manifest for ${name}@${version}` };
    }
    return { ok: true, tarball, resolved: `${body.name}@${body.version}` };
  } catch (e) {
    return { ok: false, error: String((e && /** @type {any} */ (e).message) || e) };
  }
}

/**
 * @param {any} result
 * @param {string} artifactRef
 * @returns {string}
 */
function render(result, artifactRef) {
  const lines = [`Kenwea notary — ${artifactRef}`, ""];
  if (result.checked === false) {
    lines.push(`  NOT CHECKED  ${result.reason ?? "unknown"}`);
    if (result.note) lines.push(`  ${result.note}`);
    lines.push("");
    return lines.join("\n");
  }
  lines.push(`  verdict     ${String(result.verdict ?? "unknown").toUpperCase()}`);
  if (result.verdictReason) lines.push(`  why         ${result.verdictReason}`);
  lines.push(`  sha256      ${result.contentSha256 ?? "-"}`);
  lines.push(`  size        ${result.contentSizeBytes ?? "-"} bytes`);
  lines.push(`  executed    ${result.ran ? `yes (${result.executable})` : `no${result.notRunReason ? ` — ${result.notRunReason}` : ""}`}`);
  if (result.ran) lines.push(`  exit code   ${result.exitCode}`);
  for (const [label, key] of [["secrets", "secretHits"], ["danger", "dangerHits"]]) {
    const hits = result[key];
    if (Array.isArray(hits) && hits.length) lines.push(`  ${label}     ${hits.join(", ")}`);
  }
  if (result.ran && result.output) {
    lines.push("", "  output:");
    for (const line of String(result.output).split("\n").slice(0, 20)) lines.push(`    ${line}`);
  }
  if (result.attestation) lines.push("", `  ${result.attestation}`);

  // The signature is the difference between "Kenwea says this is fine" and evidence
  // you can forward, so the CLI surfaces it rather than leaving it in the JSON where
  // only someone already looking would find it. Shown as the three things a verifier
  // needs and not the payload itself: the payload is long, and printing it in full
  // would bury the verdict the reader came for.
  const signed = result.signedAttestation;
  if (signed && signed.signature) {
    lines.push("", `  signed      ${signed.algorithm}, key ${signed.keyId}`);
    lines.push(`  verify      https://www.kenwea.com/verify`);
    lines.push(`  key         ${signed.keyUrl}`);
    lines.push("");
    lines.push("  The claim is about the sha256 above, not the URL — that address can");
    lines.push("  serve something else tomorrow. Verifying needs nothing from us.");
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
 * @param {any} body
 * @returns {string|null}
 */
function extractKey(body) {
  const parsed = extractToolResult(body);
  if (!parsed) return null;
  const raw = parsed.apiKey && parsed.apiKey.rawKey;
  return typeof raw === "string" && raw ? raw : null;
}

/**
 * @param {typeof fetch} doFetch
 * @param {BridgeConfig} config
 * @param {{sessionId: string|null}} session
 * @param {object} message
 * @returns {Promise<{ok: boolean, status: number, body: any, error: string|null}>}
 */
async function rpc(doFetch, config, session, message) {
  try {
    const res = await doFetch(config.url, {
      method: "POST",
      headers: buildHeaders(config, session, null),
      body: JSON.stringify(message),
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
