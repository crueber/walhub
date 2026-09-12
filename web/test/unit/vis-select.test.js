// web/test/unit/vis-select.test.js — Forgejo #410: VisSelect renders
// nothing (opaque render path). The profile visibility selector is the
// shared web/src/components/VisSelect.jsx, rendering the populated
// options through the REAL Solid render path
// (`<For each={visibilityOptions(props.isOrg)}>{(o) => …}</For>`,
// vite-plugin-solid against solid-js) — no render adapter, no
// JSX-runtime stub.
//
// Render-path research (verdicts; full write-up in
// docs/features/01_identity_permissions.md Decisions, #410 entry):
// - H1 CONFIRMED: the <For> children mapper is invoked per item as
//   `mapFn(item)` (client mapArray, arity-1 mapper) / `fn(item, () => i)`
//   (SSR simpleMap) — the index is an accessor function, never a raw
//   number, and the item is the option object, never `o.label`.
// - H2 CONFIRMED as an adapter bug, not framework timing: mapArray
//   reads `list() || []`, so an undefined `each` renders nothing — the
//   observed `Cannot read properties of undefined (reading 'value')`
//   comes from invoking the mapper with an undefined ITEM, which the
//   real path never does with a populated `each`.
// - H3 REFUTED for production: the vite bundle ships the runtime
//   INSIDE `/_ui/assets/*.js` (no external runtime file exists to
//   404); the 404 was a harness artifact of bypassing the vite build
//   (plain node has no JSX transform, so .jsx cannot even be
//   imported — that is the "opaque" path).
// - #4065 finding (issue comment): visibilityOptions() returned fresh
//   arrays/objects per call; <For> diffs by === identity, so every
//   owner-kind refire rebuilt all <option> nodes and the select fell
//   back to the first option (public). Fixed with hoisted constants.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

// The REAL render path: genuine solid-js <For> (under plain node this
// resolves to the SSR build — the same package, no stub adapter).
import { For, createComponent } from "solid-js/web";

import {
  visibilityOptions,
  isVisibility,
} from "../../src/lib/visibility.js";
import { visRows } from "../../src/lib/visSelect.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

// --- identity-stable options (#4065 assertion, belongs to this fix) ---

test("visibilityOptions returns identical references across calls", () => {
  assert.equal(visibilityOptions(false), visibilityOptions(false));
  assert.equal(visibilityOptions(true), visibilityOptions(true));
  assert.notEqual(visibilityOptions(false), visibilityOptions(true));
  for (const isOrg of [false, true]) {
    const a = visibilityOptions(isOrg);
    const b = visibilityOptions(isOrg);
    assert.equal(a.length, 3);
    for (let i = 0; i < 3; i++) assert.equal(a[i], b[i], `row ${i} identical`);
  }
});

test("visibilityOptions still offers the populated owner-appropriate selector", () => {
  assert.deepEqual(visibilityOptions(false), [
    { value: "public", label: "public — anyone may read" },
    { value: "authenticated", label: "private — logged in only" },
    { value: "private", label: "private — owner only" },
  ]);
  assert.deepEqual(visibilityOptions(true), [
    { value: "public", label: "public — anyone may read" },
    { value: "authenticated", label: "private — logged in only" },
    { value: "private", label: "private — org members only" },
  ]);
  for (const opts of [visibilityOptions(false), visibilityOptions(true)]) {
    for (const o of opts) assert.equal(isVisibility(o.value), true);
  }
});

// --- headless render model ---

test("visRows marks exactly the current value selected", () => {
  for (const v of ["public", "authenticated", "private"]) {
    for (const isOrg of [false, true]) {
      const rows = visRows(isOrg, v);
      assert.equal(rows.length, 3);
      assert.deepEqual(
        rows.map((r) => r.selected),
        rows.map((r) => r.value === v),
      );
      assert.equal(rows.filter((r) => r.selected).length, 1);
    }
  }
});

test("visRows selects nothing for unknown/empty values (never a public-looking default)", () => {
  for (const v of [null, undefined, "", "hidden"]) {
    for (const isOrg of [false, true]) {
      const rows = visRows(isOrg, v);
      assert.equal(rows.length, 3, "selector stays populated while unseeded");
      assert.ok(rows.every((r) => r.selected === false), `nothing selected for ${String(v)}`);
    }
  }
});

// --- live-render rig: the REAL <For>, visibility changing ---

// The production mapper shape, arity-1 `(o) => …` exactly like
// VisSelect.jsx's `{(o) => <option value={o.value}>{o.label}</option>}`
// (plus the select `value` binding mirrored as the `selected` mark).
function renderLive(isOrg, value) {
  return createComponent(For, {
    each: visibilityOptions(isOrg),
    children: (o) => ({ value: o.value, label: o.label, selected: o.value === (value ?? "") }),
  });
}

test("live-render: populated selector through the real <For>", () => {
  const rows = renderLive(false, "public");
  assert.equal(rows.length, 3);
  assert.deepEqual(
    rows.map((r) => r.value),
    ["public", "authenticated", "private"],
  );
  assert.ok(rows.every((r) => typeof r.label === "string" && r.label.length > 0));
});

test("live-render: changing visibility is reflected while changing", () => {
  let rows = renderLive(false, "public");
  assert.equal(rows.find((r) => r.selected).value, "public");
  rows = renderLive(false, "private"); // user picks private
  assert.equal(rows.find((r) => r.selected).value, "private");
  assert.equal(rows.find((r) => r.selected).label, "private — owner only");
  rows = renderLive(false, "authenticated"); // server truth moves on
  assert.equal(rows.find((r) => r.selected).value, "authenticated");
  // Owner kind flips user → org: rows stay populated, private relabels,
  // and the selected mark survives the flip (no fallback to public).
  rows = renderLive(true, "private");
  assert.equal(rows.find((r) => r.selected).label, "private — org members only");
});

test("live-render: mapper receives defined items, index as accessor (H1/H2 pins)", () => {
  const seen = [];
  const rows = createComponent(For, {
    each: visibilityOptions(false),
    children: (o, i) => {
      seen.push([o, i]);
      return o.value;
    },
  });
  assert.deepEqual(rows, ["public", "authenticated", "private"]);
  assert.equal(seen.length, 3);
  for (const [o, i] of seen) {
    assert.ok(o !== undefined && typeof o.value === "string", "item is the option object (never undefined, never o.label)");
    assert.equal(typeof i, "function", "second mapper arg is the index accessor, never a raw number");
  }
});

// --- static render-path guards (repo idiom: JSX stays DOM-thin) ---

test("VisSelect renders through the real path: <For> over visibilityOptions, arity-1 (o) mapper", () => {
  const s = srcOf("../../src/components/VisSelect.jsx");
  assert.ok(s.includes('import { For } from "solid-js"'), "real solid-js <For>, no adapter import");
  assert.ok(!s.includes("jsx-runtime"), "no throwaway JSX runtime import");
  assert.ok(!s.includes("mapArray("), "no hand-rolled mapArray adapter call");
  assert.ok(
    s.includes("<For each={visibilityOptions(props.isOrg)}>"),
    "each is the populated identity-stable array",
  );
  assert.ok(
    s.includes("{(o) => <option value={o.value}>{o.label}</option>}"),
    "mapper is arity-1 (o): option value + label, matching the real invocation shape",
  );
  assert.ok(s.includes("value={props.value ?? \"\"}"), "unseeded binds \"\" (matches no option — #394 blank select)");
});

test("Settings + Access render the shared VisSelect, no inline visibility <For> remains", () => {
  for (const rel of ["../../src/pages/Settings.jsx", "../../src/pages/Access.jsx"]) {
    const s = srcOf(rel);
    const uses = s.match(/<VisSelect[\s>]/g) ?? [];
    assert.equal(uses.length, 1, `${rel}: exactly one shared <VisSelect>`);
    assert.ok(!s.includes("visibilityOptions("), `${rel}: no inline visibilityOptions <For> left`);
  }
});
