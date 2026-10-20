import assert from "node:assert/strict";
import test from "node:test";

import { readDirects } from "../src/directs.js";

/** @param {Array<{name: string, version: string}>} list */
const ids = (list) => list.map((d) => `${d.name}@${d.version}`).join(",");

test("direct dependencies are read as the server reads them", () => {
  const lock = JSON.stringify({ lockfileVersion: 3, packages: {
    "": { dependencies: { express: "^4.21.0", alias: "npm:lodash@^4" }, devDependencies: { esbuild: "^0.28.0" } },
    "node_modules/express": { version: "4.21.2" },
    "node_modules/alias": { version: "4.17.21", name: "lodash" },
    "node_modules/esbuild": { version: "0.28.2" },
  } });
  assert.equal(ids(readDirects(lock, "package-lock.json")), "esbuild@0.28.2,express@4.21.2,lodash@4.17.21");

  const v9 = "lockfileVersion: '9.0'\n\nimporters:\n\n  .:\n    dependencies:\n      express:\n        specifier: ^4.21.0\n        version: 4.21.2\n      react-dom:\n        specifier: ^18\n        version: 18.3.1(react@18.3.1)\n    devDependencies:\n      esbuild:\n        specifier: ^0.28.0\n        version: 0.28.2\n\n  packages/other:\n    dependencies:\n      left-pad:\n        specifier: 1.3.0\n        version: 1.3.0\n\npackages:\n\n  express@4.21.2:\n    resolution: {integrity: sha512-x}\n";
  assert.equal(ids(readDirects(v9, "pnpm-lock.yaml")), "esbuild@0.28.2,express@4.21.2,react-dom@18.3.1");

  const v5 = "lockfileVersion: 5.4\n\nspecifiers:\n  express: ^4.21.0\n\ndependencies:\n  express: 4.21.2\n\ndevDependencies:\n  esbuild: 0.28.2_abc\n\npackages:\n\n  /express/4.21.2:\n    resolution: {integrity: sha512-x}\n";
  assert.equal(ids(readDirects(v5, "pnpm-lock.yaml")), "esbuild@0.28.2,express@4.21.2");
});

test("a yarn.lock is matched against the package.json beside it", () => {
  const yarn1 = '# yarn lockfile v1\n\n\nexpress@^4.18.0, express@^4.21.0:\n  version "4.21.2"\n  resolved "https://registry.yarnpkg.com/express/-/express-4.21.2.tgz"\n\nesbuild@^0.28.0:\n  version "0.28.2"\n';
  const pj = JSON.stringify({ dependencies: { express: "^4.21.0" }, devDependencies: { esbuild: "^0.28.0" } });
  assert.equal(ids(readDirects(yarn1, "yarn.lock", pj)), "esbuild@0.28.2,express@4.21.2");
  const berry = '__metadata:\n  version: 6\n\n"express@npm:^4.21.0":\n  version: 4.21.2\n  resolution: "express@npm:4.21.2"\n';
  assert.equal(ids(readDirects(berry, "yarn.lock", JSON.stringify({ dependencies: { express: "^4.21.0" } }))), "express@4.21.2");
  assert.deepEqual(readDirects(yarn1, "yarn.lock"), [], "without the package.json it cannot be said");
});
