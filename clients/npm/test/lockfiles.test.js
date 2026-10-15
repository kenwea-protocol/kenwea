import { test } from "node:test";
import assert from "node:assert/strict";

import { readAnyLockfile, askRegistry } from "../src/tree.js";

const pnpm6 = `lockfileVersion: '6.0'

packages:

  /esbuild@0.19.12:
    resolution: {integrity: sha512-def}
    requiresBuild: true

  /react-dom@18.2.0(react@18.2.0):
    resolution: {integrity: sha512-ghi}
`;

const pnpm9 = `---
lockfileVersion: '9.0'

packages:

  '@babel/core@7.24.0':
    resolution: {integrity: sha512-x}

  lodash@4.17.21:
    resolution: {integrity: sha512-y}
`;

const yarn1 = `# yarn lockfile v1


lodash@^4.17.21:
  version "4.17.21"
  resolved "https://registry.yarnpkg.com/lodash/-/lodash-4.17.21.tgz#679591"
  integrity sha512-b

string-width-cjs@npm:string-width@^4.2.0:
  version "4.2.3"
  resolved "https://registry.yarnpkg.com/string-width/-/string-width-4.2.3.tgz#269c71"
  integrity sha512-c
`;

const berry = `__metadata:
  version: 8

"lodash@npm:^4.17.21":
  version: 4.17.21
  resolution: "lodash@npm:4.17.21"

"my-app@workspace:.":
  resolution: "my-app@workspace:."
`;

const ids = (r) => r.entries.map((e) => `${e.name}@${e.version}=${e.runs === undefined ? "?" : e.runs}`).sort().join(" ");

test("readAnyLockfile reads pnpm and yarn as the server does", () => {
  assert.equal(ids(readAnyLockfile(pnpm6)), "esbuild@0.19.12=true react-dom@18.2.0=false");
  assert.equal(readAnyLockfile(pnpm6).entries[0].integrity, "sha512-def");
  assert.equal(ids(readAnyLockfile(pnpm9)), "@babel/core@7.24.0=? lodash@4.17.21=?");
  const y = readAnyLockfile(yarn1);
  assert.equal(y.format, "yarn.lock");
  assert.equal(ids(y), "lodash@4.17.21=? string-width@4.2.3=?");
  assert.equal(y.entries.find((e) => e.name === "lodash").resolved, "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz");
  assert.equal(ids(readAnyLockfile(berry)), "lodash@4.17.21=?");
  assert.equal(readAnyLockfile("hello").ok, false);
});

test("askRegistry fills what a lockfile does not say, and leaves the rest unknown", async () => {
  const lock = readAnyLockfile(pnpm9);
  const seen = [];
  const fake = async (url) => {
    seen.push(url);
    if (url.endsWith("/lodash/4.17.21")) return new Response(JSON.stringify({ version: "4.17.21", scripts: { test: "x" } }));
    return new Response("not found", { status: 404 });
  };
  await askRegistry(fake, lock.entries, "https://registry.example");
  assert.equal(ids(lock), "@babel/core@7.24.0=? lodash@4.17.21=false");
  assert.ok(seen.every((u) => u.startsWith("https://registry.example/")), "only the registry is asked");
});

test("sriMatches takes the strongest algorithm listed, sha1 included", async () => {
  const { sriMatches } = await import("../src/check.js");
  const { createHash } = await import("node:crypto");
  const b = Buffer.from("tarball");
  const sha1 = "sha1-" + createHash("sha1").update(b).digest("base64");
  const sha512 = "sha512-" + createHash("sha512").update(b).digest("base64");
  assert.equal(sriMatches(sha1, b), true);
  assert.equal(sriMatches(sha512, b), true);
  assert.equal(sriMatches(`${sha1} sha512-AAAA`, b), false, "a wrong sha512 is not rescued by a right sha1");
  assert.equal(sriMatches("sha1-AAAA", b), false);
  assert.equal(sriMatches("md5-xyz", b), false);
});
