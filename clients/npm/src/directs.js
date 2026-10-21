// The project's own dependencies, read from what is on this machine (0.7.0).
//
// A step runs inside the project, under the package that pulled it in:
// @scarf/scarf reports install telemetry only for the package that depends on
// it, and alone it sends nothing. So the notary checks a tree through its direct
// dependencies, each with its whole tree, and the packages under one are
// answered by its record. This reads which those are: the root of a
// package-lock.json, every importer of a pnpm-lock.yaml (each workspace package
// is a project of its own; 0.7.1) or v5's top level,
// and for a yarn.lock, which does not say, the package.json beside it matched
// against the lockfile's entries. Their names and versions go to the notary
// with the packages that run something.

/** @param {string} s */
function unquote(s) {
  s = s.trim();
  return s.length >= 2 && (s[0] === '"' || s[0] === "'") && s[s.length - 1] === s[0] ? s.slice(1, -1) : s;
}

/** @param {Array<{name: string, version: string}>} list */
function unique(list) {
  const seen = new Set();
  const out = [];
  for (const d of list) {
    const id = `${d.name}@${d.version}`;
    if (d.name && d.version && !seen.has(id)) {
      seen.add(id);
      out.push(d);
    }
  }
  return out.sort((a, b) => (`${a.name}@${a.version}` < `${b.name}@${b.version}` ? -1 : 1));
}

/**
 * @param {string} text the lockfile
 * @param {string} format as readAnyLockfile said
 * @param {string} [packageJson] the package.json beside it, for a yarn.lock
 * @returns {Array<{name: string, version: string}>} empty when it cannot be said
 */
export function readDirects(text, format, packageJson) {
  if (format === "package-lock.json") return packageLockDirects(text);
  if (format === "pnpm-lock.yaml") return pnpmDirects(text);
  if (format === "yarn.lock" && packageJson) return yarnDirects(text, packageJson);
  return [];
}

/** @param {string} text */
function packageLockDirects(text) {
  /** @type {any} */
  let doc;
  try {
    doc = JSON.parse(text);
  } catch (e) {
    return [];
  }
  const pkgs = doc && doc.packages;
  const root = pkgs && pkgs[""];
  if (!root) return [];
  /** @type {Array<{name: string, version: string}>} */
  const out = [];
  for (const group of [root.dependencies, root.devDependencies, root.optionalDependencies]) {
    for (const name of Object.keys(group || {})) {
      const p = pkgs[`node_modules/${name}`];
      if (p && p.version) out.push({ name: typeof p.name === "string" && p.name ? p.name : name, version: String(p.version) });
    }
  }
  return unique(out);
}

/** @param {string} text */
function pnpmDirects(text) {
  /** @type {Array<{name: string, version: string}>} */
  const out = [];
  let section = "", inRoot = false, group = "", name = "";
  const add = (/** @type {string} */ n, /** @type {string} */ v) => {
    v = unquote(v);
    const cut = v.search(/[(_]/);
    if (cut > 0) v = v.slice(0, cut);
    if (n && v && !v.startsWith("link:") && !v.startsWith("file:")) out.push({ name: unquote(n), version: v });
  };
  const groups = new Set(["dependencies", "devDependencies", "optionalDependencies"]);
  for (const line of text.split(/\r?\n/)) {
    if (!line.trim() || line.trim().startsWith("#")) continue;
    const indent = line.length - line.trimStart().length;
    const t = line.trim();
    const colon = t.indexOf(":");
    const key = unquote(colon >= 0 ? t.slice(0, colon) : t);
    const val = colon >= 0 ? t.slice(colon + 1).trim() : "";
    if (indent === 0) {
      section = key;
      group = groups.has(key) ? key : "";
      inRoot = !!group;
      name = "";
    } else if (section === "importers" && indent === 2) {
      inRoot = true;
      group = name = "";
    } else if (section === "importers" && inRoot && indent === 4) {
      group = groups.has(key) ? key : "";
      name = "";
    } else if (section === "importers" && inRoot && group && indent === 6) {
      if (val) add(key, val);
      else name = key;
    } else if (section === "importers" && inRoot && group && indent === 8 && key === "version" && name) {
      add(name, val);
      name = "";
    } else if (section !== "importers" && inRoot && group && indent === 2 && val) {
      add(key, val);
    }
  }
  return unique(out);
}

/**
 * A yarn.lock keys each entry by the specs that resolved to it ("express@^4.21.0"
 * in v1, "express@npm:^4.21.0" in berry); the package.json says which specs are
 * the project's own.
 * @param {string} text @param {string} packageJson
 */
function yarnDirects(text, packageJson) {
  /** @type {any} */
  let pj;
  try {
    pj = JSON.parse(packageJson);
  } catch (e) {
    return [];
  }
  /** @type {Map<string, string>} spec -> version */
  const specs = new Map();
  /** @type {string[]} */
  let keys = [];
  for (const line of text.split(/\r?\n/)) {
    if (!line || line.startsWith("#")) continue;
    if (line[0] !== " ") {
      keys = line.replace(/:\s*$/, "").split(",").map((k) => unquote(k)).filter(Boolean);
      continue;
    }
    const t = line.trim();
    const m = /^version:?\s+"?([^"\s]+)"?$/.exec(t);
    if (m) for (const k of keys) specs.set(k, m[1]);
  }
  /** @type {Array<{name: string, version: string}>} */
  const out = [];
  for (const group of [pj.dependencies, pj.devDependencies, pj.optionalDependencies]) {
    for (const [name, range] of Object.entries(group || {})) {
      const v = specs.get(`${name}@${range}`) || specs.get(`${name}@npm:${range}`);
      if (v) out.push({ name, version: v });
    }
  }
  return unique(out);
}
