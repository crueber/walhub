// web/test/unit/commit-graph-506.test.js — Forgejo #506: optional
// commit-graph visualization on the Commits tab. The lane derivation is the
// pure web/src/lib/commit-graph.js module (the settingsNav convention — no
// Solid, no DOM); the page wiring (toggle, memo, rail, collapse) is pinned
// as source text, mirroring checks-tab-505.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import {
  GRAPH_STORAGE_KEY,
  GRAPH_LANES,
  GRAPH_LANE_GAP,
  GRAPH_LANE_PAD,
  readGraphEnabled,
  writeGraphEnabled,
  laneClass,
  laneX,
  railWidth,
  assignLanes,
  rowDiagonals,
} from "../../src/lib/commit-graph.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const COMMITS = srcOf("../../src/pages/Commits.jsx");
const CSS = srcOf("../../src/ui.css");
const PKG = JSON.parse(srcOf("../../package.json"));

const C = (sha, parents = []) => ({ sha, parents });

// --- lane assignment ---

test("empty window derives empty (never throws on surprises)", () => {
  assert.deepEqual(assignLanes([]), { rows: [], width: 0 });
  assert.deepEqual(assignLanes(undefined), { rows: [], width: 0 });
  assert.deepEqual(assignLanes(null), { rows: [], width: 0 });
  const one = assignLanes([C("a")]);
  assert.equal(one.width, 1);
  assert.equal(one.rows.length, 1);
  assert.equal(one.rows[0].lane, 0);
  assert.deepEqual(one.rows[0].top, [null]);
  assert.deepEqual(one.rows[0].bottom, []);
});

test("linear history threads one lane (first-parent stays put)", () => {
  const { rows, width } = assignLanes([C("c", ["b"]), C("b", ["a"]), C("a")]);
  assert.equal(width, 1);
  assert.deepEqual(rows.map((r) => r.lane), [0, 0, 0]);
  for (const r of rows) {
    assert.deepEqual(rowDiagonals(r), [], `no diagonals: ${r.sha}`);
  }
  assert.deepEqual(rows[0].bottom, ["b"]);
  assert.deepEqual(rows[1].top, ["b"]);
});

test("branch: second tip opens a lane and rejoins the base below", () => {
  const { rows, width } = assignLanes([C("side", ["base"]), C("main", ["base"]), C("base")]);
  assert.equal(width, 2);
  assert.deepEqual(rows.map((r) => r.lane), [0, 1, 0]);
  // The second tip's parent is already carried on lane 0, so its row
  // carries the rejoin as a diagonal — no phantom lane survives to base.
  assert.deepEqual(rowDiagonals(rows[1]), [{ from: 1, to: 0 }], "side tip rejoins below");
  assert.deepEqual(rowDiagonals(rows[2]), [], "base row runs straight");
});

test("merge: extra parent fans out below, side lane joins back", () => {
  const commits = [C("m", ["a", "b"]), C("a", ["x"]), C("b", ["x"]), C("x")];
  const { rows, width } = assignLanes(commits);
  assert.equal(width, 2);
  assert.deepEqual(rows.map((r) => r.lane), [0, 0, 1, 0]);
  assert.deepEqual(rows[0].bottom, ["a", "b"], "both parents carried below the merge");
  assert.deepEqual(rowDiagonals(rows[0]), [{ from: 0, to: 1 }], "diverging line to the side lane");
  assert.deepEqual(rowDiagonals(rows[2]), [{ from: 1, to: 0 }], "side lane joins the base below");
});

test("branch-then-merge keeps first-parent threading on lane 0", () => {
  const commits = [
    C("tip", ["m"]),
    C("m", ["main", "feat"]),
    C("main", ["root"]),
    C("feat", ["root"]),
    C("root"),
  ];
  const { rows, width } = assignLanes(commits);
  assert.equal(width, 2);
  assert.deepEqual(rows.map((r) => r.lane), [0, 0, 0, 1, 0]);
  assert.deepEqual(rows[1].parents, ["main", "feat"]);
  assert.deepEqual(rowDiagonals(rows[1]), [{ from: 0, to: 1 }]);
  assert.deepEqual(rowDiagonals(rows[3]), [{ from: 1, to: 0 }], "side lane rejoins the base below");
});

test("per-window derivation: every window opens on lane 0 (pager trade-off)", () => {
  // Window 2 knows nothing of window 1's lanes — its first row still
  // threads lane 0, and unknown parents take fresh lanes, never -1.
  const { rows, width } = assignLanes([C("old2", ["old1"]), C("old1", ["older"])]);
  assert.equal(width, 1);
  assert.deepEqual(rows.map((r) => r.lane), [0, 0]);
  assert.deepEqual(rows[1].bottom, ["older"]);
});

// --- geometry helpers ---

test("rail geometry: lane centers pad out, width fits a phone viewport", () => {
  assert.equal(laneX(0), GRAPH_LANE_PAD);
  assert.equal(laneX(1), GRAPH_LANE_PAD + GRAPH_LANE_GAP);
  assert.equal(railWidth(1), GRAPH_LANE_PAD * 2);
  assert.equal(railWidth(4), GRAPH_LANE_PAD * 2 + 3 * GRAPH_LANE_GAP);
  // 8 lanes at 12px + padding = 100px: the rail never pushes 390px sideways.
  assert.ok(railWidth(GRAPH_LANES) <= 120, `8-lane rail is narrow: ${railWidth(GRAPH_LANES)}px`);
  for (let i = 0; i < 16; i++) {
    assert.match(laneClass(i), /^gl-[0-7]$/, `lane ${i} cycles the palette`);
  }
  assert.equal(laneClass(8), "gl-0");
});

// --- toggle persistence (theme precedent) ---

function withStorage(value, fn) {
  const had = "localStorage" in globalThis;
  const prev = globalThis.localStorage;
  if (value === "throw") {
    globalThis.localStorage = {
      getItem() { throw new Error("denied"); },
      setItem() { throw new Error("denied"); },
      removeItem() { throw new Error("denied"); },
    };
  } else {
    let store = value;
    globalThis.localStorage = {
      getItem: (k) => (k === GRAPH_STORAGE_KEY ? store : null),
      setItem: (k, v) => { if (k === GRAPH_STORAGE_KEY) store = String(v); },
      removeItem: (k) => { if (k === GRAPH_STORAGE_KEY) store = null; },
      _get: () => store,
    };
  }
  try {
    const result = fn();
    const stored = globalThis.localStorage._get?.();
    return { result, stored };
  } finally {
    if (had) globalThis.localStorage = prev;
    else delete globalThis.localStorage;
  }
}

test("toggle defaults OFF, persists on, clears on off", () => {
  assert.equal(withStorage(null, () => readGraphEnabled()).result, false, "fresh visitor: OFF");
  assert.equal(withStorage("0", () => readGraphEnabled()).result, false, "any non-1 value: OFF");
  const on = withStorage(null, () => {
    writeGraphEnabled(true);
    return readGraphEnabled();
  });
  assert.equal(on.result, true);
  assert.equal(on.stored, "1");
  const off = withStorage("1", () => {
    writeGraphEnabled(false);
    return readGraphEnabled();
  });
  assert.equal(off.result, false);
  assert.equal(off.stored, null);
});

test("storage unavailable (or no localStorage) falls back to OFF, never throws", () => {
  assert.equal(withStorage("throw", () => readGraphEnabled()).result, false);
  withStorage("throw", () => writeGraphEnabled(true)); // must not throw
  const had = "localStorage" in globalThis;
  const prev = globalThis.localStorage;
  if (had) delete globalThis.localStorage;
  try {
    assert.equal(readGraphEnabled(), false);
    writeGraphEnabled(true); // must not throw
  } finally {
    if (had) globalThis.localStorage = prev;
  }
});

// --- page wiring (source pins) ---

test("Commits.jsx: toggle lives in the crumbs row, persisted, default OFF", () => {
  assert.ok(COMMITS.includes("../lib/commit-graph.js"), "page imports the lane module");
  assert.ok(COMMITS.includes("readGraphEnabled"), "initial state reads the persisted toggle");
  assert.ok(COMMITS.includes("writeGraphEnabled"), "flips persist");
  assert.ok(COMMITS.includes("aria-pressed={graphOn()}"), "toggle exposes pressed state");
  assert.ok(COMMITS.includes('class="pill cursor-pointer"'), "toggle speaks the pill idiom");
  assert.ok(COMMITS.includes("graph-on"), "list carries the graph-aware grid class");
});

test("Commits.jsx: lanes memoize per window off the existing cache (no new fetch)", () => {
  assert.ok(COMMITS.includes("assignLanes(h()?.commits"), "derivation reads the sha+path+skip window");
  assert.ok(COMMITS.includes("createMemo"), "derivation is memoized, not recomputed per row");
  assert.ok(COMMITS.includes("sha+path+skip"), "the per-window key is documented at the memo");
  const fetches = (COMMITS.match(/\.commits\(/g) ?? []).length;
  assert.equal(fetches, 1, "exactly one commits fetch — the graph adds zero requests");
});

test("Commits.jsx: rail renders lane color class-only, keeps ParentLinks", () => {
  assert.ok(COMMITS.includes("GraphRail"), "row renders the lane rail component");
  assert.ok(COMMITS.includes("stroke=\"currentColor\""), "svg paints via currentColor, never literals");
  assert.ok(COMMITS.includes("ParentLinks"), "the parent text links survive (mobile fallback)");
  assert.ok(!COMMITS.includes('stroke="#'), "no hex stroke literals in the page");
  assert.ok(!COMMITS.includes('fill="#'), "no hex fill literals in the page");
  assert.ok(COMMITS.includes("graph-dot-merge"), "merge commits get a distinct node");
  assert.ok(COMMITS.includes("restarts the lane layout"), "the pager trade-off is called out in the UI");
});

test("ui.css: themed lane palette (light + dark), graph grid, phone collapse", () => {
  for (let i = 0; i < GRAPH_LANES; i++) {
    assert.ok(CSS.includes(`.gl-${i}`), `lane class .gl-${i} defined`);
    assert.ok(CSS.includes(`--graph-${i}:`), `lane var --graph-${i} defined`);
  }
  assert.ok(CSS.includes(".dark"), "dark theme carries its own palette");
  assert.ok(CSS.includes(".graph-on .commit-row"), "graph-aware grid variant defined");
  assert.ok(CSS.includes("@media (max-width: 480px)"), "phone collapse defined");
  const collapse = CSS.slice(CSS.indexOf("@media (max-width: 480px)"));
  assert.ok(collapse.includes(".commit-rail") && collapse.includes("display: none"), "rail hides on phones");
});

test("no new dependencies (law 1: hand-rolled lanes, zero packages)", () => {
  assert.deepEqual(
    Object.keys(PKG.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps unchanged",
  );
});
