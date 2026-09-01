// web/test/unit/reactive.test.js — the §2.1 reactive core contract.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createSignal, createEffect, createMemo, onCleanup, createRoot } from "../../src/lib/reactive.js";

test("effect reruns on signal change", () => {
  const [get, set] = createSignal(1);
  let runs = 0;
  createRoot(() => {
    createEffect(() => { get(); runs++; });
    set(2);
    assert.equal(runs, 2);
  });
});

test("effect runs immediately on creation", () => {
  let runs = 0;
  createRoot(() => { createEffect(() => { runs++; }); });
  assert.equal(runs, 1);
});

test("set with an updater function", () => {
  const [get, set] = createSignal(5);
  set((prev) => prev * 2);
  assert.equal(get(), 10);
});

test("set to the same value does not notify", () => {
  const [get, set] = createSignal("x");
  let runs = 0;
  createRoot(() => {
    createEffect(() => { get(); runs++; });
    set("x");
  });
  assert.equal(runs, 1);
  assert.equal(get(), "x");
});

test("notifications are synchronous (no batching)", () => {
  const [get, set] = createSignal(0);
  const seen = [];
  createRoot(() => {
    createEffect(() => seen.push(get()));
    set(1);
    set(2);
  });
  assert.deepEqual(seen, [0, 1, 2]);
});

test("effect tracks dependencies dynamically per run", () => {
  const [a, setA] = createSignal(true);
  const [x, setX] = createSignal("x");
  const [y, setY] = createSignal("y");
  let seen = "";
  createRoot(() => {
    createEffect(() => { seen = a() ? x() : y(); });
    setX("X");
    assert.equal(seen, "X");
    setY("Y"); // y is not a dependency in this run: no change
    assert.equal(seen, "X");
    setA(false);
    assert.equal(seen, "Y");
    setX("X2"); // x dropped from deps: no change
    assert.equal(seen, "Y");
  });
});

test("memo is cached until a dependency changes, then lazy", () => {
  const [get, set] = createSignal(2);
  let computes = 0;
  let dbl;
  createRoot(() => { dbl = createMemo(() => { computes++; return get() * 2; }); });
  assert.equal(dbl(), 4);
  assert.equal(dbl(), 4);
  assert.equal(computes, 1);
  set(3);
  assert.equal(computes, 1); // lazy: not recomputed eagerly
  assert.equal(dbl(), 6);
  assert.equal(computes, 2);
  assert.equal(dbl(), 6);
  assert.equal(computes, 2);
});

test("memo chains propagate", () => {
  const [get, set] = createSignal(1);
  createRoot(() => {
    const m1 = createMemo(() => get() + 1);
    const m2 = createMemo(() => m1() * 10);
    const seen = [];
    createEffect(() => seen.push(m2()));
    set(2);
    assert.deepEqual(seen, [20, 30]);
  });
});

test("effect depending only on a memo reruns when the memo's source changes", () => {
  const [get, set] = createSignal(1);
  const seen = [];
  createRoot(() => {
    const m = createMemo(() => get() * 2);
    createEffect(() => seen.push(m()));
    set(5);
  });
  assert.deepEqual(seen, [2, 10]);
});

test("onCleanup runs on effect re-run and root dispose", () => {
  const [get, set] = createSignal(0);
  const log = [];
  const dispose = createRoot(() => {
    createEffect(() => {
      const v = get();
      onCleanup(() => log.push(`cleanup@${v}`));
    });
  });
  set(1);
  assert.deepEqual(log, ["cleanup@0"]);
  dispose();
  assert.deepEqual(log, ["cleanup@0", "cleanup@1"]);
});

test("createRoot dispose stops effects and is idempotent", () => {
  const [get, set] = createSignal(0);
  let runs = 0;
  let cleanups = 0;
  const dispose = createRoot(() => {
    createEffect(() => { get(); runs++; });
    onCleanup(() => cleanups++);
  });
  assert.equal(runs, 1);
  dispose();
  dispose();
  set(2);
  assert.equal(runs, 1);
  assert.equal(cleanups, 1);
});

test("effects created inside a root are torn down with it", () => {
  const [get, set] = createSignal(0);
  let inner = 0;
  const dispose = createRoot(() => { createEffect(() => { get(); inner++; }); });
  assert.equal(inner, 1);
  dispose();
  set(9);
  assert.equal(inner, 1);
});

test("nested effect: set inside an effect re-runs dependents synchronously", () => {
  const [get, set] = createSignal(0);
  let runs = 0;
  createRoot(() => {
    createEffect(() => { runs++; if (get() < 3) set(get() + 1); });
  });
  assert.equal(get(), 3);
  assert.ok(runs >= 3);
});

test("signal array updates via updater", () => {
  const [get, set] = createSignal([]);
  set((prev) => [...prev, "a"]);
  set((prev) => [...prev, "b"]);
  assert.deepEqual(get(), ["a", "b"]);
});

test("no scope: signals still work, onCleanup is a no-op", () => {
  const [get, set] = createSignal(1);
  set(2);
  assert.equal(get(), 2);
  onCleanup(() => { throw new Error("must not run"); });
});
