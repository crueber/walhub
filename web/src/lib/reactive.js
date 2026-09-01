// web/src/lib/reactive.js — the §2.1 reactive core (normative API) + the two DOM
// wiring helpers every page uses (bind/el). Importable in Node with no DOM access.

// --- core ------------------------------------------------------------------

let current = null; // computation (effect/memo) currently running
const rootStack = []; // enclosing createRoot scopes

function flushCleanups(comp) {
  const cs = comp.cleanups;
  comp.cleanups = [];
  for (const fn of cs) fn();
}

function unsubscribeAll(comp) {
  for (const s of comp.deps) s.subs.delete(comp);
  comp.deps.clear();
}

function run(comp) {
  if (comp.disposed) return;
  flushCleanups(comp);
  unsubscribeAll(comp);
  const prev = current;
  current = comp;
  try {
    comp.value = comp.fn();
  } finally {
    current = prev;
  }
}

function notify(comp) {
  if (comp.disposed) return;
  if (comp.kind === "effect") return run(comp);
  comp.dirty = true;
  for (const c of [...comp.subs]) notify(c);
}

/** → [get, set]; get() reads, set(v) or set(prev => next) writes. */
export function createSignal(initial) {
  const sig = { subs: new Set(), value: initial };
  const get = () => {
    if (current) {
      current.deps.add(sig);
      sig.subs.add(current);
    }
    return sig.value;
  };
  const set = (v) => {
    const next = typeof v === "function" ? v(sig.value) : v;
    if (Object.is(next, sig.value)) return;
    sig.value = next;
    for (const c of [...sig.subs]) notify(c);
  };
  return [get, set];
}

/** Runs fn immediately; re-runs synchronously when any signal read inside changes. */
export function createEffect(fn) {
  const comp = { kind: "effect", fn, deps: new Set(), subs: new Set(), cleanups: [], disposed: false };
  const root = rootStack[rootStack.length - 1];
  if (root) root.children.push(comp);
  run(comp);
  return comp;
}

/** Derived value, cached until a dependency changes; recomputed lazily on next read. */
export function createMemo(fn) {
  const comp = { kind: "memo", fn, deps: new Set(), subs: new Set(), cleanups: [], dirty: true, value: undefined, disposed: false };
  const root = rootStack[rootStack.length - 1];
  if (root) root.children.push(comp);
  const get = () => {
    if (current && current !== comp) {
      current.deps.add(comp);
      comp.subs.add(current);
    }
    if (comp.dirty) {
      comp.dirty = false;
      run(comp);
    }
    return comp.value;
  };
  return get;
}

/** Registers teardown for the current computation scope (if any). */
export function onCleanup(fn) {
  const target = current ?? rootStack[rootStack.length - 1];
  if (target) target.cleanups.push(fn);
}

/** Scoped root: fn(dispose); teardown on dispose. */
export function createRoot(fn) {
  const root = { kind: "root", deps: new Set(), subs: new Set(), cleanups: [], children: [], disposed: false };
  rootStack.push(root);
  let done = false;
  const dispose = () => {
    if (done) return;
    done = true;
    root.disposed = true;
    flushCleanups(root);
    unsubscribeAll(root);
    for (const c of root.children) {
      c.disposed = true;
      flushCleanups(c);
      unsubscribeAll(c);
    }
    root.children.length = 0;
  };
  const prev = current;
  current = null;
  try {
    fn(dispose);
  } finally {
    current = prev;
    rootStack.pop();
  }
  return dispose;
}

// --- DOM wiring helpers (§2.1 "Templates") ----------------------------------

/**
 * Clone a <template> and bind elements by data-* attribute.
 * map: { [dataName]: { onclick?, onchange?, textContent?, value?, className?, ... } }
 * Returns { [dataName]: element }.
 */
export function bind(scope, map) {
  const bound = {};
  for (const [name, spec] of Object.entries(map ?? {})) {
    const node = scope.querySelector(`[data-${name}]`);
    if (!node) continue;
    bound[name] = node;
    for (const [k, v] of Object.entries(spec ?? {})) {
      if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2).toLowerCase(), v);
      else node[k] = v;
    }
  }
  return bound;
}

/** Create an element: el("a", { class: "sha", href: "/x" }, "text", childNode). */
export function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === undefined || v === null || v === false) continue;
      if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === "class") node.className = v;
      else if (k === "dataset") Object.assign(node.dataset, v);
      else if (v === true) node.setAttribute(k, "");
      else node.setAttribute(k, String(v));
    }
  }
  for (const child of children.flat(Infinity)) {
    if (child === undefined || child === null || child === false) continue;
    node.append(child.nodeType ? child : document.createTextNode(String(child)));
  }
  return node;
}
