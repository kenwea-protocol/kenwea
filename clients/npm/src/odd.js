// What the signed record knows that its one line does not say (2026-11-12).
//
// vina, yuigui, johnnybucks and trustlessmolty on Moltbook: a record whose
// signature verifies and whose level is quiet can still hold a load that
// failed, a program a step looked for and did not find, or a trace that was
// cut. Those sit in their own fields, and the line a reader sees says
// "verified" and "quiet" together. This lists them, from the signed facts only,
// so anyone holding the same record gets the same list. nx 24.0.5 is the case:
// quiet and approved, while its import failed and a step looked for git.
//
// Not odd, on purpose: a phase that was skipped with a reason (nothing to run,
// no dependencies), and a load that failed because the entry is a stylesheet or
// the code expects a browser, which the record already explains and which says
// nothing about the package. A phase blocked by an earlier one is counted once,
// at the earlier one.
//
// The Claude Code plugin keeps an identical copy in scripts/lib.mjs, so it does
// not depend on a new release of this package. Change both together.

const EXPECTED_LOAD = [/entry is a \.\w+ file/, /expects a browser/];
const NOT_DONE = new Set(["failed", "blocked", "not_reached"]);

/** @param {string} cause */
function short(cause) {
  const first = cause.split(";")[0].trim();
  return first.length > 90 ? first.slice(0, 90) + "..." : first;
}

/** @param {any} o @returns {string[]} */
function missingOf(o) {
  return o && typeof o === "object" && Array.isArray(o.missing) ? o.missing.map(String) : [];
}

/**
 * The fields of a signed record that a reader should look at although the
 * level does not say so. Empty means there is nothing else to look at.
 * @param {any} f the signed facts (the parsed signedAttestation.payload)
 * @returns {string[]}
 */
export function oddFields(f) {
  /** @type {string[]} */
  const odd = [];
  if (!f || typeof f !== "object") return odd;
  for (const p of Array.isArray(f.phases) ? f.phases : []) {
    if (!p || typeof p !== "object" || p.blockedBy || !NOT_DONE.has(p.status)) continue;
    const cause = typeof p.cause === "string" ? p.cause : "";
    if (p.name === "load" && EXPECTED_LOAD.some((r) => r.test(cause))) continue;
    odd.push(`${String(p.name)} ${String(p.status).replace("_", " ")}${cause ? ` (${short(cause)})` : ""}`);
  }
  const load = f.load && typeof f.load === "object" ? f.load : {};
  const missing = [...new Set([...missingOf(f.observed), ...missingOf(load.observed)])];
  if (missing.length) odd.push(`a step looked for ${missing.join(", ")} and did not find it`);
  if ((f.observed && f.observed.truncated) || (load.observed && load.observed.truncated)) odd.push("the trace was cut, so it may be incomplete");
  const lost = Number(f.sandbox && f.sandbox.traceLost);
  if (lost > 0) odd.push(`${lost} trace events were lost`);
  if (f.dependencies && f.dependencies.complete === false) odd.push("the dependency tree was not complete");
  return odd;
}
