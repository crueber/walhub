// web/src/lib/commit-graph.js — Forgejo #506: optional commit-graph lanes.
//
// Pure module (no Solid, no DOM): lane assignment derives client-side from
// the existing {sha, parents} commit payload — zero new network requests,
// zero backend/SDK/wire changes. The settingsNav / diff.js precedent:
// headless-testable, covered by `node --test` without a DOM.
//
// Per-window derivation (documented trade-off): the commits window is
// sha-addressed and paged (?skip=), so lanes derive from the VISIBLE window
// only. Crossing the "older →" pager restarts the lane layout — the first
// row of every window opens on lane 0. The toggle defaults OFF, so the user
// opts into that trade-off; Commits.jsx calls this out under the pager.

/** localStorage key for the graph toggle (theme precedent: try/catch, OFF fallback). */
export const GRAPH_STORAGE_KEY = "walhub-commit-graph";

/** Palette size: ui.css defines .gl-0..7 + --graph-* vars for light + dark. */
export const GRAPH_LANES = 8;

/** px between lane centers in the rail svg; side padding on each side. */
export const GRAPH_LANE_GAP = 12;
export const GRAPH_LANE_PAD = 8;

/** svg viewBox height units per row (top half 0–24, node at 24, bottom 24–48). */
export const GRAPH_ROW_H = 48;
export const GRAPH_ROW_MID = 24;

/**
 * readGraphEnabled() → boolean: the persisted toggle, default OFF.
 * Fresh visitors (no saved value) and storage-unavailable environments
 * both read OFF — the store.js theme-pattern fallback.
 */
export function readGraphEnabled() {
  try {
    if (typeof localStorage === "undefined") return false;
    return localStorage.getItem(GRAPH_STORAGE_KEY) === "1";
  } catch {
    return false;
  }
}

/** writeGraphEnabled(on): persist the toggle; never throws into the page. */
export function writeGraphEnabled(on) {
  try {
    if (typeof localStorage === "undefined") return;
    if (on) localStorage.setItem(GRAPH_STORAGE_KEY, "1");
    else localStorage.removeItem(GRAPH_STORAGE_KEY);
  } catch {
    /* non-fatal */
  }
}

/**
 * laneClass(lane) → the themed color class for a lane index (cycles the
 * 8-entry palette). Color lives in ui.css vars — no literals here or in JSX.
 */
export function laneClass(lane) {
  const n = Number(lane);
  const k = Number.isFinite(n) ? Math.abs(Math.trunc(n)) % GRAPH_LANES : 0;
  return `gl-${k}`;
}

/** laneX(lane) → x-center of a lane in the rail svg. */
export function laneX(lane) {
  return GRAPH_LANE_PAD + lane * GRAPH_LANE_GAP;
}

/** railWidth(width) → svg width for a graph of `width` lanes. */
export function railWidth(width) {
  return GRAPH_LANE_PAD * 2 + Math.max(0, width - 1) * GRAPH_LANE_GAP;
}

/**
 * assignLanes(commits) → {rows, width}: first-parent lane threading over
 * the visible window (newest first, the CommitPage order).
 *
 * Each row: {sha, lane, parents, top, bottom} where top/bottom are the
 * lane-boundary snapshots (sha | null per lane) entering/leaving the row.
 * Convergence renders as bottom diagonals on the rejoining row (a side
 * lane's first parent already carried below); divergence renders as bottom
 * diagonals on the branching row (extra parents fanning out). Straight
 * verticals need no entry — the rail draws those from top/bottom.
 *
 * Rules: a commit reuses the lane already carrying its sha, else takes the
 * first free lane (branch-out); its first parent continues on its lane
 * unless that parent is already carried below (diagonal join); every other
 * parent takes a free lane or converges (merge-in). Commits with no parents
 * close their lane. Non-array input (and rows without shas) behave as an
 * empty window — never throw on server-shaped surprises.
 */
export function assignLanes(commits) {
  const rows = [];
  const list = Array.isArray(commits) ? commits : [];
  let lanes = [];
  let width = 0;
  const snap = () => {
    if (lanes.length > width) width = lanes.length;
    return lanes.slice();
  };

  for (const c of list) {
    const sha = String(c?.sha ?? "");
    const parents = Array.isArray(c?.parents) ? c.parents.map((p) => String(p)) : [];
    // Reuse the lane already carrying this sha, else take the first free
    // lane (branch-out). Placement below never duplicates a sha across
    // lanes, so no converge-from-above bookkeeping is needed.
    let idx = lanes.indexOf(sha);
    if (idx === -1) {
      const free = lanes.indexOf(null);
      if (free === -1) {
        idx = lanes.length;
        lanes.push(null);
      } else {
        idx = free;
      }
    }
    const top = snap();
    // Advance the boundary: first parent continues on idx (or joins below),
    // other parents each take a lane (or converge when already carried).
    const [first, ...rest] = parents;
    if (first === undefined) {
      lanes[idx] = null;
    } else {
      const m = lanes.indexOf(first);
      if (m === -1) lanes[idx] = first;
      else if (m !== idx) lanes[idx] = null;
    }
    for (const p of rest) {
      if (lanes.indexOf(p) !== -1) continue;
      const free = lanes.indexOf(null);
      if (free === -1) lanes.push(p);
      else lanes[free] = p;
    }
    while (lanes.length > 0 && lanes[lanes.length - 1] === null) lanes.pop();
    const bottom = snap();
    rows.push({ sha, lane: idx, parents, top, bottom });
  }
  return { rows, width };
}

/**
 * rowDiagonals(row) → slanted {from, to} lane pairs within the row's lower
 * half, node → parent lane: branch-out on extra parents plus the merge-in
 * join when a first parent is already carried on another lane below.
 */
export function rowDiagonals(row) {
  const lane = row?.lane ?? 0;
  const bottom = [];
  const parents = Array.isArray(row?.parents) ? row.parents : [];
  const laneOf = (sha) => (Array.isArray(row?.bottom) ? row.bottom.indexOf(sha) : -1);
  for (const p of parents) {
    const m = laneOf(p);
    if (m !== -1 && m !== lane && !bottom.some((d) => d.to === m)) bottom.push({ from: lane, to: m });
  }
  return bottom;
}
