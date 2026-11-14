import assert from "node:assert/strict";
import test from "node:test";

import { oddFields } from "../src/odd.js";

// Signed payloads as production issued them on 2026-11-10 to 2026-11-12, cut to
// the fields oddFields reads.
const ok = (name) => ({ name, status: "completed" });
const quietObs = { dns: [], missing: [], network: [], programs: ["node"], truncated: false };

test("nx 24.0.5: quiet and approved, yet its import failed and a step looked for git", () => {
  const f = {
    level: "quiet", verdict: "approved",
    phases: [ok("unpack"), ok("dependencies"), ok("installSteps"),
      { name: "load", status: "failed", cause: "it exited 1; many libraries need configuration or a host to load, so this says little either way" }],
    observed: { ...quietObs, missing: ["git"] },
    load: { ran: true, exitCode: 1, observed: { ...quietObs, missing: ["git"] } },
    sandbox: { traceLost: 0 }, dependencies: { complete: true },
  };
  assert.deepEqual(oddFields(f), ["load failed (it exited 1)", "a step looked for git and did not find it"]);
});

test("a stylesheet package and a browser library are explained, not odd", () => {
  for (const cause of [
    "it exited 1 because its entry is a .css file, which Node does not import; a stylesheet package has no code that runs on import",
    "it exited 1 because it expects a browser (Node said: window is not defined); the load runs in Node, so its browser code was not exercised",
  ]) {
    const f = {
      phases: [ok("unpack"), { name: "dependencies", status: "skipped", cause: "nothing runs at install anywhere in its tree" },
        { name: "installSteps", status: "skipped", cause: "it declares no install step and ships no binding.gyp" },
        { name: "load", status: "failed", cause }],
      load: { ran: true, exitCode: 1, observed: quietObs },
    };
    assert.deepEqual(oddFields(f), []);
  }
});

test("a quiet record with everything completed has nothing else to look at", () => {
  const f = { phases: [ok("unpack"), ok("dependencies"), ok("installSteps"), ok("load")], observed: quietObs,
    load: { ran: true, exitCode: 0, observed: quietObs }, sandbox: { traceLost: 0 }, dependencies: { complete: true } };
  assert.deepEqual(oddFields(f), []);
});

test("a blocked phase is counted once, at the phase that blocked it", () => {
  const f = {
    phases: [ok("unpack"), ok("dependencies"),
      { name: "installSteps", status: "failed", cause: "it exited 1; with no network and no native build tools here, it may have failed for that reason" },
      { name: "load", status: "not_reached", blockedBy: "installSteps" }],
    observed: { ...quietObs, missing: ["python", "python3"] },
  };
  assert.deepEqual(oddFields(f), ["installSteps failed (it exited 1)", "a step looked for python, python3 and did not find it"]);
});

test("a cut trace, lost events and an incomplete tree are each named", () => {
  const f = { phases: [ok("unpack")], observed: { ...quietObs, truncated: true }, sandbox: { traceLost: 3 }, dependencies: { complete: false } };
  assert.deepEqual(oddFields(f), ["the trace was cut, so it may be incomplete", "3 trace events were lost", "the dependency tree was not complete"]);
});

test("Python phases use the same rule", () => {
  const f = { phases: [ok("unpack"), ok("dependencies"),
    { name: "imports", status: "failed", cause: "it exited 1; with no network and no compiler here" },
    { name: "entryPoint", status: "not_reached", blockedBy: "imports" }] };
  assert.deepEqual(oddFields(f), ["imports failed (it exited 1)"]);
});

test("nothing to read gives an empty list, never a throw", () => {
  for (const f of [null, undefined, "x", 3, {}, { phases: "no" }, { phases: [null, 3, {}] }]) assert.deepEqual(oddFields(f), []);
});
