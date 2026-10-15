// Kenwea MCP bridge: `kenwea check-tree [lockfile]` (0.5.0; pnpm and yarn 0.6.0).
//
// A whole dependency tree in one call. The lockfile is read HERE, and only the
// packages it says run something at install (hasInstallScript, usually a few
// percent) are sent to the notary; the names of the rest never leave this
// machine. Each one comes back with its stored signed record, or queued for a
// background check. Every record is verified here exactly as `check` verifies
// one: the signature under the key it names, refusing a revoked key, and the
// tarball npm installs matched against the lockfile's integrity and the
// record's sha256. Gates read the signed facts. A queued package has no record
// yet, so a gate cannot pass on it: --wait polls until it has one.

import { readFile, access } from "node:fs/promises";

import { EXIT, KEYS_URL, gateRefusals, matchTarball, normaliseGates, notaryUrl, rpc, sanitize, extractToolResult, verifyRecord } from "./check.js";

/**
 * @typedef {import("./config.js").BridgeConfig} BridgeConfig
 */

const POLL_MS = 30_000;

/**
 * The packages of a package-lock.json (lockfileVersion 2 or 3).
 * @param {string} text
 * @returns {{ok: true, all: number, runs: Array<{name: string, version: string, resolved: string, integrity: string}>} | {ok: false, error: string}}
 */
export function readLockfile(text) {
  /** @type {any} */
  let doc;
  try {
    doc = JSON.parse(text);
  } catch (e) {
    return { ok: false, error: "not JSON" };
  }
  if (!doc || typeof doc !== "object" || !doc.packages || !(doc.lockfileVersion >= 2)) {
    return { ok: false, error: `lockfileVersion ${doc && doc.lockfileVersion} has no hasInstallScript flags; regenerate it with npm 7 or later (npm install --package-lock-only)` };
  }
  const seen = new Set();
  const runs = [];
  let all = 0;
  for (const [key, p] of Object.entries(doc.packages)) {
    const at = key.lastIndexOf("node_modules/");
    if (!key || at < 0 || !p || p.link || !p.version) continue;
    const name = typeof p.name === "string" && p.name ? p.name : key.slice(at + "node_modules/".length);
    const id = `${name}@${p.version}`;
    if (seen.has(id)) continue;
    seen.add(id);
    all++;
    if (p.hasInstallScript === true) {
      runs.push({ name, version: String(p.version), resolved: typeof p.resolved === "string" ? p.resolved : "", integrity: typeof p.integrity === "string" ? p.integrity : "" });
    }
  }
  return { ok: true, all, runs };
}

const NPM_NAME = /^(@[a-z0-9~-][a-z0-9._~-]*\/)?[a-z0-9~-][a-z0-9._~-]*$/;
const SEMVER = /^\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]*)?$/;

/** @param {string} s */
function unquote(s) {
  s = s.trim();
  if (s.length >= 2 && (s[0] === '"' || s[0] === "'") && s[s.length - 1] === s[0]) return s.slice(1, -1);
  return s;
}

/** @param {string} s */
function splitNameVersion(s) {
  const i = s.lastIndexOf("@");
  return i <= 0 ? ["", ""] : [s.slice(0, i), s.slice(i + 1)];
}

/**
 * Any lockfile (0.6.0): a package-lock.json, a pnpm-lock.yaml (v5, v6, v9) or a
 * yarn.lock (v1, berry). Entries the lockfile does not mark as running something
 * or not (pnpm v9, yarn) come back with runs undefined, to be asked of the npm
 * registry from this machine; never of Kenwea.
 * @param {string} text
 * @returns {{ok: true, format: string, entries: Array<{name: string, version: string, resolved: string, integrity: string, runs: boolean | undefined}>} | {ok: false, error: string}}
 */
export function readAnyLockfile(text) {
  const t = text.replace(/^\uFEFF/, "").trim().replace(/^---\s*/, "");
  if (t.startsWith("{")) {
    let doc;
    try {
      doc = JSON.parse(text);
    } catch (e) {
      return { ok: false, error: "not JSON" };
    }
    if (!doc || typeof doc !== "object" || !doc.packages || !(doc.lockfileVersion >= 2)) {
      return { ok: false, error: `lockfileVersion ${doc && doc.lockfileVersion} has no hasInstallScript flags; regenerate it with npm 7 or later (npm install --package-lock-only)` };
    }
    const seen = new Set();
    const entries = [];
    for (const [key, p] of Object.entries(doc.packages)) {
      const at = key.lastIndexOf("node_modules/");
      if (!key || at < 0 || !p || p.link || !p.version) continue;
      const name = typeof p.name === "string" && p.name ? p.name : key.slice(at + "node_modules/".length);
      const id = `${name}@${p.version}`;
      if (seen.has(id)) continue;
      seen.add(id);
      entries.push({ name, version: String(p.version), resolved: typeof p.resolved === "string" ? p.resolved : "", integrity: typeof p.integrity === "string" ? p.integrity : "", runs: p.hasInstallScript === true });
    }
    return { ok: true, format: "package-lock.json", entries };
  }
  const lines = t.split(/\r?\n/);
  if (t.startsWith("lockfileVersion:")) {
    const version = unquote(lines[0].slice("lockfileVersion:".length));
    const flags = !version.startsWith("9");
    const entries = [];
    const seen = new Map();
    let inPackages = false;
    let cur = -1;
    for (const line of lines) {
      if (line && line[0] !== " " && line[0] !== "#") {
        inPackages = line.trim() === "packages:";
        cur = -1;
        continue;
      }
      if (!inPackages) continue;
      if (/^  \S/.test(line) && line.trim().endsWith(":")) {
        let key = unquote(line.trim().slice(0, -1)).replace(/^\//, "");
        const paren = key.indexOf("(");
        if (paren > 0) key = key.slice(0, paren);
        let name = "", ver = "";
        if (version.startsWith("5")) {
          const i = key.lastIndexOf("/");
          if (i > 0) [name, ver] = [key.slice(0, i), key.slice(i + 1).split("_")[0]];
        } else [name, ver] = splitNameVersion(key);
        cur = -1;
        if (!SEMVER.test(ver) || !NPM_NAME.test(name)) continue;
        const id = `${name}@${ver}`;
        if (seen.has(id)) { cur = seen.get(id); continue; }
        entries.push({ name, version: ver, resolved: "", integrity: "", runs: flags ? false : undefined });
        cur = entries.length - 1;
        seen.set(id, cur);
        continue;
      }
      if (cur < 0) continue;
      const tl = line.trim();
      if (tl === "requiresBuild: true" && flags) entries[cur].runs = true;
      const integ = /integrity: (sha512-[A-Za-z0-9+/=]+)/.exec(tl);
      if (integ) entries[cur].integrity = integ[1];
    }
    return { ok: true, format: "pnpm-lock.yaml", entries };
  }
  if (text.includes("# yarn lockfile v1")) {
    const entries = [];
    const seen = new Set();
    let name = "", ver = "", resolved = "", integrity = "";
    const flush = () => {
      const m = /^https:\/\/registry\.(?:yarnpkg\.com|npmjs\.org)\/((?:@[^/]+\/)?[^/]+)\/-\/[^/]+-(\d+\.\d+\.\d+[^/]*)\.tgz/.exec(resolved);
      if (m && m[2] === ver) name = m[1];
      if (SEMVER.test(ver) && NPM_NAME.test(name) && !seen.has(`${name}@${ver}`)) {
        seen.add(`${name}@${ver}`);
        entries.push({ name, version: ver, resolved: resolved.split("#")[0].replace("https://registry.yarnpkg.com/", "https://registry.npmjs.org/"), integrity, runs: undefined });
      }
      name = ver = resolved = integrity = "";
    };
    for (const line of lines) {
      if (!line || line.startsWith("#")) continue;
      if (line[0] !== " ") {
        flush();
        [name] = splitNameVersion(unquote(line.replace(/:$/, "").split(",")[0]));
        continue;
      }
      const tl = line.trim();
      if (tl.startsWith("version ")) ver = unquote(tl.slice(8));
      else if (tl.startsWith("resolved ")) resolved = unquote(tl.slice(9));
      else if (tl.startsWith("integrity ")) integrity = tl.slice(10).trim();
    }
    flush();
    return { ok: true, format: "yarn.lock", entries };
  }
  if (text.includes("__metadata:")) {
    const entries = [];
    const seen = new Set();
    for (const line of lines) {
      const tl = line.trim();
      if (!tl.startsWith("resolution:")) continue;
      const res = unquote(tl.slice("resolution:".length));
      const i = res.indexOf("@npm:");
      if (i <= 0 || res.startsWith("patch:")) continue;
      const name = res.slice(0, i), ver = res.slice(i + 5);
      if (!SEMVER.test(ver) || !NPM_NAME.test(name) || seen.has(`${name}@${ver}`)) continue;
      seen.add(`${name}@${ver}`);
      entries.push({ name, version: ver, resolved: "", integrity: "", runs: undefined });
    }
    return { ok: true, format: "yarn.lock", entries };
  }
  return { ok: false, error: "not a lockfile this reads: package-lock.json (v2, v3), pnpm-lock.yaml and yarn.lock (v1 and berry) are" };
}

/**
 * Asks the npm registry, from this machine, whether each version runs anything
 * at install: a preinstall, install or postinstall script, or a binding.gyp.
 * Only the npm registry sees these names, as it does when you install.
 * @param {typeof fetch} doFetch
 * @param {Array<{name: string, version: string, runs: boolean | undefined}>} entries
 * @param {string} [registry]
 */
export async function askRegistry(doFetch, entries, registry = "https://registry.npmjs.org") {
  const todo = entries.filter((e) => e.runs === undefined);
  let next = 0;
  const deadline = Date.now() + 60_000;
  const worker = async () => {
    while (next < todo.length && Date.now() < deadline) {
      const e = todo[next++];
      try {
        const res = await doFetch(`${registry}/${e.name.replace("/", "%2F")}/${encodeURIComponent(e.version)}`, { signal: AbortSignal.timeout(15_000) });
        if (!res.ok) continue;
        const m = /** @type {any} */ (await res.json());
        if (!m || m.version !== e.version) continue;
        const scripts = m.scripts && typeof m.scripts === "object" ? m.scripts : {};
        e.runs = !!m.gypfile || ["preinstall", "install", "postinstall"].some((s) => typeof scripts[s] === "string" && scripts[s].trim() !== "");
      } catch (err) {
        /* left unknown, and said so */
      }
    }
  };
  await Promise.all(Array.from({ length: 16 }, worker));
}

/**
 * @param {BridgeConfig} config
 * @param {string} path
 * @param {{fetchImpl?: typeof fetch, stdout?: NodeJS.WritableStream, stderr?: NodeJS.WritableStream, sleep?: (ms: number) => Promise<void>}} [io]
 * @param {{failOn?: string[], json?: boolean, waitSeconds?: number, keysUrl?: string}} [opts]
 * @returns {Promise<{code: number}>}
 */
export async function runCheckTree(config, path, io = {}, opts = {}) {
  const doFetch = io.fetchImpl ?? fetch;
  const out = io.stdout ?? process.stdout;
  const err = io.stderr ?? process.stderr;
  const sleep = io.sleep ?? ((ms) => new Promise((r) => setTimeout(r, ms)));
  const gates = normaliseGates(opts.failOn);
  let file = path;
  if (!file) {
    for (const f of ["package-lock.json", "pnpm-lock.yaml", "yarn.lock"]) {
      try {
        await access(f);
        file = f;
        break;
      } catch (e) {
        /* try the next */
      }
    }
    file = file || "package-lock.json";
  }

  let text;
  try {
    text = await readFile(file, "utf8");
  } catch (e) {
    err.write(`[kenwea-mcp] could not read ${sanitize(file)}: ${sanitize(/** @type {any} */ (e).message)}\n`);
    return { code: EXIT.USAGE };
  }
  const parsed = readAnyLockfile(text);
  if (!parsed.ok) {
    err.write(`[kenwea-mcp] ${sanitize(file)}: ${sanitize(parsed.error)}\n`);
    return { code: EXIT.USAGE };
  }
  if (parsed.entries.some((e) => e.runs === undefined)) {
    err.write(`[kenwea-mcp] ${sanitize(parsed.format)} does not say which packages run something at install; asking the npm registry\n`);
    await askRegistry(io.registryFetch ?? doFetch, parsed.entries);
  }
  const unknown = parsed.entries.filter((e) => e.runs === undefined);
  const lock = { all: parsed.entries.length, runs: parsed.entries.filter((e) => e.runs === true), unknown: unknown.length, format: parsed.format };

  const deadline = Date.now() + Math.max(0, opts.waitSeconds ?? 0) * 1000;
  /** @type {any} */
  let tree = null;
  for (;;) {
    tree = lock.runs.length ? await askTree(doFetch, config, lock.runs) : { packages: [] };
    if (!tree.ok && lock.runs.length) {
      err.write(`[kenwea-mcp] the notary did not answer: ${sanitize(tree.error)}\n`);
      return { code: EXIT.NO_ANSWER };
    }
    const waiting = (tree.packages || []).filter((/** @type {any} */ p) => p.status === "queued").length;
    if (!waiting || Date.now() + POLL_MS > deadline) break;
    err.write(`[kenwea-mcp] ${waiting} package(s) queued for a check; asking again in ${POLL_MS / 1000} s\n`);
    await sleep(POLL_MS);
  }

  // Verify every answered record here, before anything is said or gated on it.
  const byId = new Map(lock.runs.map((r) => [`${r.name}@${r.version}`, r]));
  /** @type {Array<{name: string, version: string, status: string, facts?: any, item?: any, error?: string, refusals?: string[]}>} */
  const rows = [];
  for (const item of tree.packages || []) {
    const row = { name: String(item.name), version: String(item.version), status: String(item.status), item };
    if (item.status === "answered") {
      const checked = await verifyRecord(doFetch, item, String(item.artifactRef), opts.keysUrl ?? KEYS_URL);
      const lockEntry = byId.get(`${row.name}@${row.version}`);
      if (!checked.ok) {
        rows.push({ ...row, status: "unverified", error: checked.error });
        continue;
      }
      if (lockEntry && lockEntry.integrity) {
        const match = await matchTarball(doFetch, { tarball: String(item.artifactRef), integrity: lockEntry.integrity, shasum: "" }, String(checked.facts.contentSha256 ?? ""));
        if (!match.ok) {
          rows.push({ ...row, status: "unverified", error: `not about the bytes your lockfile installs: ${match.error}` });
          continue;
        }
      }
      rows.push({ ...row, facts: checked.facts, refusals: gateRefusals(gates, checked.facts) });
    } else {
      rows.push(row);
    }
  }

  if (opts.json) {
    out.write(JSON.stringify({ lockfile: file, format: lock.format, packages: lock.all, runSomething: lock.runs.length, unknown: lock.unknown, results: rows.map(({ item, ...r }) => r) }) + "\n");
  } else {
    out.write(render(file, lock, rows));
  }

  const unverified = rows.filter((r) => r.status === "unverified");
  for (const r of unverified) err.write(`[kenwea-mcp] ${sanitize(r.name)}@${sanitize(r.version)}: the record did not verify: ${sanitize(r.error)}\n`);
  if (unverified.length) return { code: EXIT.UNVERIFIED };
  if (!gates.length) return { code: EXIT.OK };
  if (lock.unknown) err.write(`[kenwea-mcp] gate: ${lock.unknown} package(s) could not be classified, so --fail-on ${gates.join(",")} cannot pass\n`);
  const pending = rows.filter((r) => r.status !== "answered");
  for (const r of pending) err.write(`[kenwea-mcp] gate: ${sanitize(r.name)}@${sanitize(r.version)} has no record yet (${sanitize(r.status)}), so --fail-on ${gates.join(",")} cannot pass; try --wait 600\n`);
  const refused = rows.filter((r) => r.refusals && r.refusals.length);
  for (const r of refused) for (const why of r.refusals || []) err.write(`[kenwea-mcp] gate: ${sanitize(r.name)}@${sanitize(r.version)}: ${why}\n`);
  if (refused.length) return { code: EXIT.GATE };
  if (pending.length || lock.unknown) return { code: EXIT.NO_ANSWER };
  return { code: EXIT.OK };
}

/**
 * @param {typeof fetch} doFetch
 * @param {BridgeConfig} config
 * @param {Array<{name: string, version: string}>} runs
 */
async function askTree(doFetch, config, runs) {
  const active = { ...config, url: notaryUrl(config.url) };
  const session = { sessionId: null };
  const res = await rpc(doFetch, active, session, {
    jsonrpc: "2.0",
    id: "check-tree",
    method: "tools/call",
    params: { name: "kenwea_notary_check_tree", arguments: { packages: runs.map((r) => ({ name: r.name, version: r.version, hasInstallScript: true })) } },
  }, 60_000);
  if (!res.ok) return { ok: false, error: res.error || `HTTP ${res.status}` };
  const result = extractToolResult(res.body);
  if (res.body && res.body.result && res.body.result.isError) {
    return { ok: false, error: `${(result && result.error) || "refused"}: ${(result && result.detail) || ""}` };
  }
  if (!result || typeof result !== "object") return { ok: false, error: "no readable result" };
  return { ok: true, ...result };
}

/** @param {string} file @param {{all: number, runs: any[], unknown?: number, format?: string}} lock @param {any[]} rows */
function render(file, lock, rows) {
  const answered = rows.filter((r) => r.facts);
  const queued = rows.filter((r) => r.status === "queued" || r.status === "not_queued");
  const lines = [`Kenwea notary: ${sanitize(file)}`, ""];
  const said = lock.format === "package-lock.json" ? "your lockfile says so" : "your lockfile or, where it does not say, the npm registry, asked from here";
  lines.push(`  ${lock.all} packages; ${lock.all - lock.runs.length - (lock.unknown || 0)} run nothing of their own at install (${said}; their names did not go to Kenwea).`);
  if (lock.unknown) lines.push(`  ${lock.unknown} could not be asked of the registry in time and are not checked.`);
  lines.push(`  ${lock.runs.length} run something: ${answered.length} with a verified signed record, ${queued.length} not checked yet.`, "");
  const order = { sends_credentials: 0, reads_credentials: 1, network: 2, unfinished: 3, quiet: 4, nothing_runs: 5 };
  answered.sort((a, b) => ((/** @type {any} */ (order))[a.facts.level] ?? 3) - ((/** @type {any} */ (order))[b.facts.level] ?? 3));
  for (const r of answered) {
    const level = String(r.facts.level || r.facts.verdict || "").toUpperCase();
    lines.push(`  ${level.padEnd(18)} ${sanitize(r.name)}@${sanitize(r.version)}`);
    if (r.item && r.item.summary && r.facts.level !== "quiet") lines.push(`  ${" ".repeat(18)} ${sanitize(r.item.summary)}`);
  }
  for (const r of queued) lines.push(`  ${"QUEUED".padEnd(18)} ${sanitize(r.name)}@${sanitize(r.version)}`);
  for (const r of rows.filter((x) => x.status === "unverified")) lines.push(`  ${"NOT VERIFIED".padEnd(18)} ${sanitize(r.name)}@${sanitize(r.version)}`);
  if (queued.length) lines.push("", "  Queued packages are checked in the background, about two a minute. Run again later, or add --wait 600.");
  lines.push("", "  Each record is signed and was verified here against the published keys:", `  ${KEYS_URL}`, "");
  return lines.join("\n");
}
