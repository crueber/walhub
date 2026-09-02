// web/src/lib/data.js — the §2.4 data layer: promise-cache with TTL revalidation,
// sha-addressed forever-cache, LRU cap, global error tray state and top-progress
// counter. Solid signals power it; App.jsx wires the progress bar + tray effects.
// D-WEB-6 (2026-09-02): same API on Solid primitives after the vanilla-ESM port.

import { createSignal, createEffect } from "solid-js";

export const DEFAULT_TTL = 5_000; // revalidation window for ref-dependent data
const MAX_ENTRIES = 400; // LRU cap
const TRAY_MAX = 6; // error tray entries
const TRAY_FADE_MS = 10_000; // auto-fade

export const REPO_TTL = 5_000; // repo context per §2.6
export const RESOLVE_TTL = 5_000; // resolve step of §9.2
export const SHA_TTL = Infinity; // sha-addressed payloads are immutable

// --- pending counter (top progress bar) --------------------------------------

const [pendingGet, pendingSet] = createSignal(0);
/** Global in-flight counter (fetches + dynamic imports); the progress bar effect reads it. */
export function usePending() {
  return pendingGet;
}

/** Track a promise (fetch or dynamic import) in the pending counter. */
export function trackPending(promise) {
  pendingSet((n) => n + 1);
  Promise.resolve(promise).finally(() => pendingSet((n) => Math.max(0, n - 1)));
  return promise;
}

// --- error tray (max 6, deduped by key+message, auto-fade) -------------------

const [errorsGet, errorsSet] = createSignal([]);
/** Signal: [{key, message, at}] — the tray element in main.js renders this. */
export function trayErrors() {
  return errorsGet();
}

/** Push an error into the global tray; never throws into the page. */
export function reportError(err, key = "") {
  const message = String(err?.message ?? err ?? "unknown error");
  errorsSet((list) => {
    if (list.some((e) => e.key === key && e.message === message)) return list; // deduped
    const entry = { key, message, at: Date.now() };
    const next = [...list, entry].slice(-TRAY_MAX);
    setTimeout(() => errorsSet((cur) => cur.filter((e) => e !== entry)), TRAY_FADE_MS);
    return next;
  });
}

export function dismissError(entry) {
  errorsSet((list) => list.filter((e) => e !== entry));
}

// --- the promise cache --------------------------------------------------------

const cache = new Map(); // key → {signal, promise, value, at, error}

function touch(key, entry) {
  cache.delete(key);
  cache.set(key, entry); // re-insert = most recently used
}

function evict() {
  for (const [key, entry] of cache) {
    if (cache.size <= MAX_ENTRIES) break;
    if (entry.promise) continue; // never drop an in-flight entry
    cache.delete(key);
  }
}

function start(key, entry, fn) {
  entry.refetch = fn;
  entry.promise = trackPending(
    (async () => {
      try {
        const value = await fn();
        entry.value = value;
        entry.at = Date.now();
        entry.error = null;
        entry.signal[1](value);
      } catch (err) {
        entry.error = err;
        entry.at = Date.now();
        reportError(err, key); // errors go to the tray, not into the page
      } finally {
        entry.promise = null;
      }
    })()
  );
}

function ensureEntry(key) {
  let entry = cache.get(key);
  if (!entry) {
    entry = { signal: createSignal(undefined), promise: null, value: undefined, at: 0, error: null, refetch: null };
    cache.set(key, entry);
  }
  return entry;
}

function startIfStale(key, entry, fn, ttl) {
  touch(key, entry);
  evict();
  const fresh = entry.at > 0 && (ttl === Infinity || Date.now() - entry.at <= ttl);
  if (!entry.promise && !fresh) start(key, entry, fn);
}

/**
 * useData(key, fn, ttl?) → [get]. A promise-cache entry keyed by string;
 * TTL revalidation (default 5s; pass Infinity for sha-addressed payloads).
 * The key may be a GETTER (reactive): when it changes, the returned getter
 * re-points at the new entry — same-route param changes never go stale.
 */
export function useData(key, fn, ttl = DEFAULT_TTL) {
  const keyOf = () => (typeof key === "function" ? key() : key);
  const [getEntry, setEntry] = createSignal(undefined);
  createEffect(() => {
    const k = keyOf();
    const entry = ensureEntry(k);
    startIfStale(k, entry, fn, ttl);
    setEntry(entry);
  });
  return [() => getEntry()?.signal[0]()];
}

/** Force a refetch of one key (e.g. after a mutating call). */
export function invalidate(key) {
  const entry = cache.get(key);
  if (entry && !entry.promise) start(key, entry, entry.refetch ?? (() => Promise.resolve(entry.value)));
}

/** Internal: remember fn so invalidate() can refetch. */
export function useDataRefetchable(key, fn, ttl) {
  return useData(key, fn, ttl);
}

// --- §9.2 the resolve → sha-addressed chain ----------------------------------
// The SDK client is injected by main.js (`initData(repos)`) so this module stays
// importable in Node without resolving the bare "repos" specifier (§5 headless rule).
let repos = null;
/** Wire the SDK client (called once from main.js). */
export function initData(client) {
  repos = client;
}



function shaFetcher(owner, name, kind, r) {
  if (!repos) throw new Error("data layer not initialized: call initData(repos)");
  const repo = repos.repo(`${owner}/${name}`);
  const path = r.path ?? "";
  switch (kind) {
    case "tree":
      return () => repo.tree(r.sha, path);
    case "blob":
      return () => repo.blob(r.sha, path);
    case "commits":
      return () => repo.commits({ ref: r.sha, path });
    case "commit":
      return () => repo.commit(r.sha);
    default:
      throw new Error(`useResolved: unknown kind ${kind}`);
  }
}

/**
 * useResolved(owner, name, rest, kind) → [get]: the two-step §9.2 idiom.
 * Step 1 resolve:{rest} (ref-dependent, 5s SWR); step 2 sha:{sha}:{kind}:{path}
 * with ttl = Infinity (immutable). The chain IS the idiom.
 */
export function useResolved(owner, name, rest, kind) {
  const v = (x) => (typeof x === "function" ? x() : x);
  const [getOut, setOut] = createSignal(undefined);
  createEffect(() => {
    const o = v(owner), n = v(name), rv = v(rest) ?? "";
    const repo = `${o}/${n}`;
    // Step 1: resolve (ref-dependent, 5s SWR).
    const rKey = `resolve:${repo}/${rv}`;
    const rEntry = ensureEntry(rKey);
    startIfStale(rKey, rEntry, () => repos.repo(repo).resolve(rv), RESOLVE_TTL);
    const r = rEntry.signal[0]();
    if (!r || !r.sha) return setOut(undefined);
    // Step 2: sha-addressed payload (immutable).
    const sKey = `sha:${r.sha}:${kind}:${r.path ?? ""}`;
    const sEntry = ensureEntry(sKey);
    startIfStale(sKey, sEntry, shaFetcher(o, n, kind, r), SHA_TTL);
    const out = sEntry.signal[0]();
    // Sha-addressed payloads are ref-free by design (§2.4); the UI builds
    // URLs from the ref the user resolved, so attach it here.
    setOut(out && !out.ref ? { ...out, ref: r.ref } : out);
  });
  return [getOut, setOut]; // same [get] contract as useData
}
