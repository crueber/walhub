// web/test/unit/commits-page-529.test.js — Forgejo #529: the Commits tab
// page-2 (+skip) window called useData(...) INSIDE a createEffect body.
// useData opens its own createEffect, so invoking it during an effect run
// nested its scope under the outer effect and every re-run grew the effect
// graph without bound — RangeError: Maximum call stack size exceeded — and
// the page then hung on "loading history…" forever (a failed useData entry
// carries only entry.error, never a value, so h() stayed undefined).
//
// Solid effects do not run under the solid-js server build, so the page
// wiring is pinned as source text (the commit-graph-506.test.js convention)
// while the failure semantics the UI now handles (failed entry keeps its
// value undefined; invalidate() retry recovers) are driven headlessly
// through the real ensureEntry → start → invalidate path via prefetchData.

import { test, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

import { prefetchData, invalidate, trayErrors } from "../../src/lib/data.js";

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const COMMITS = srcOf("../../src/pages/Commits.jsx");

// Under mocked timers setTimeout never fires, so the data-layer tests below
// flush microtasks instead (the start() settle path is promise-only; the
// only timer involved is reportError's fade, which stays pending and mocked).
const flush = async (n = 10) => {
  for (let i = 0; i < n; i++) await Promise.resolve();
};

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

// --- structure: no hook instantiation inside any effect body ----------------
// A createEffect body (braced or single-expression) must never contain a
// useData/useResolved/useDataRefetchable call — the hook opens its own
// effect, and nesting it under a running effect is the #529 recursion.
function effectBodies(src) {
  const bodies = [];
  for (const m of src.matchAll(/createEffect\(/g)) {
    let j = src.indexOf("(", m.index);
    let depth = 0, instr = null;
    const start = j;
    while (j < src.length) {
      const c = src[j];
      if (instr) {
        if (c === instr && src[j - 1] !== "\\") instr = null;
      } else if (c === '"' || c === "'" || c === "`") instr = c;
      else if (c === "(") depth++;
      else if (c === ")") {
        depth--;
        if (depth === 0) break;
      }
      j++;
    }
    bodies.push({ body: src.slice(start, j + 1), line: src.slice(0, m.index).split("\n").length });
  }
  return bodies;
}

test("Commits.jsx: no useData/useResolved call lives inside a createEffect body", () => {
  const hooks = ["useData(", "useResolved(", "useDataRefetchable("];
  for (const { body, line } of effectBodies(COMMITS)) {
    for (const hook of hooks) {
      assert.ok(!body.includes(hook), `createEffect near line ${line} instantiates ${hook}`);
    }
  }
});

test("sweep: no page or component nests a data hook inside an effect", () => {
  const pages = fs.readdirSync(new URL("../../src/pages/", import.meta.url));
  const hooks = ["useData(", "useResolved(", "useDataRefetchable("];
  for (const name of pages) {
    if (!name.endsWith(".jsx")) continue;
    const src = srcOf(`../../src/pages/${name}`);
    for (const { body, line } of effectBodies(src)) {
      for (const hook of hooks) {
        assert.ok(!body.includes(hook), `${name} createEffect near line ${line} instantiates ${hook}`);
      }
    }
  }
});

test("Commits.jsx: page-2 window is a hoisted reactive-key subscription", () => {
  assert.ok(
    COMMITS.includes("() => pageKey() ?? \"commits:none\""),
    "useData key is reactive with a do-not-fetch sentinel (the DocTabs pattern)",
  );
  assert.ok(COMMITS.includes("if (!k) return Promise.resolve(null)"), "sentinel key resolves null, never fetches");
  const fetches = (COMMITS.match(/\.commits\(/g) ?? []).length;
  assert.equal(fetches, 1, "exactly one commits fetch — the refactor adds zero requests");
  assert.ok(
    COMMITS.includes("`sha:${first.sha}:commits:${path()}:${skip()}`"),
    "window key stays sha+path+skip addressed (immutable per sha)",
  );
});

test("Commits.jsx: page-1 path unchanged (resolve → sha first window)", () => {
  assert.ok(COMMITS.includes('useResolved(props.owner, props.name, props.rest, "commits")'), "page 1 still resolves");
  assert.ok(COMMITS.includes("skip() === 0 && !path()"), "first-window condition kept");
  assert.ok(COMMITS.includes("pageKey() ? getPage() : getFirst()"), "h() serves getFirst() whenever no window is keyed");
});

test("Commits.jsx: a failed window renders an inline error with retry, never an eternal spinner", () => {
  assert.ok(COMMITS.includes('role="alert"'), "error notice is a live region");
  assert.ok(COMMITS.includes("Could not load this page of history."), "human-readable message, no raw error strings");
  assert.ok(COMMITS.includes("onClick={retryPage}"), "retry button wired");
  assert.ok(COMMITS.includes("if (k) invalidate(k)"), "retry invalidates the cached window (DegradedNotice precedent)");
  assert.ok(COMMITS.includes("pageKey() && pageError()"), "error shows only while its window is active");
  assert.ok(COMMITS.includes("loading history…"), "pending windows still pulse");
});

test("Commits.jsx: graph lanes still derive per visible window", () => {
  assert.ok(COMMITS.includes("assignLanes(h()?.commits"), "derivation reads the same window getter");
});

// --- data-layer semantics the fix relies on ---------------------------------

test("a failed page-2 window keeps its value undefined and trays the keyed error", async () => {
  mock.timers.enable({ apis: ["setTimeout"] }); // neutralize reportError's 10 s fade timer
  try {
    const d = deferred();
    const get = prefetchData("sha:abc123:commits::35", () => d.promise);
    d.reject(new Error("boom"));
    await flush();
    assert.equal(get(), undefined, "failed entries carry only entry.error, never a value");
    assert.ok(
      trayErrors().some((e) => e.key === "sha:abc123:commits::35" && e.message === "boom"),
      "the rejection trays under its cache key",
    );
  } finally {
    mock.timers.reset();
  }
});

test("invalidate() retry after a failed window recovers the value", async () => {
  mock.timers.enable({ apis: ["setTimeout"] });
  try {
    const d1 = deferred(), d2 = deferred();
    const bodies = [() => d1.promise, () => d2.promise];
    let n = 0;
    const get = prefetchData("sha:abc123:commits::70", () => bodies[n++]());
    d1.reject(new Error("flaky"));
    await flush();
    assert.equal(get(), undefined);
    invalidate("sha:abc123:commits::70"); // what the Retry button drives
    d2.resolve({ commits: [{ sha: "abc123" }], more: false });
    await flush();
    assert.deepEqual(get(), { commits: [{ sha: "abc123" }], more: false });
  } finally {
    mock.timers.reset();
  }
});
