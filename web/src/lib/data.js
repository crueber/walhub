// web/src/lib/data.js — the §2.4 data layer: promise-cache with TTL revalidation,
// sha-addressed forever-cache, LRU cap, global error tray state and top-progress
// counter. Signal-only: the DOM (progress bar element, tray element) is wired in
// main.js via effects; this module stays importable without a DOM.

import { createSignal, createEffect } from "./reactive.js";

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

/**
 * useData(key, fn, ttl?) → [get]. A promise-cache entry keyed by string;
 * TTL revalidation (default 5s; pass Infinity for sha-addressed payloads).
 * Background refresh keeps stale data on screen; failures go to the tray.
 */
export function useData(key, fn, ttl = DEFAULT_TTL) {
  let entry = cache.get(key);
  if (!entry) {
    entry = { signal: createSignal(undefined), promise: null, value: undefined, at: 0, error: null };
    cache.set(key, entry);
  }
  touch(key, entry);
  evict();
  const fresh = entry.at > 0 && (ttl === Infinity || Date.now() - entry.at <= ttl);
  if (!entry.promise && !fresh) start(key, entry, fn);
  return entry.signal;
}

/** Force a refetch of one key (e.g. after a mutating call). */
export function invalidate(key) {
  const entry = cache.get(key);
  if (entry && !entry.promise) start(key, entry, entry.refetch ?? (() => Promise.resolve(entry.value)));
}

/** Internal: remember fn so invalidate() can refetch. */
export function useDataRefetchable(key, fn, ttl) {
  const entry = cache.get(key);
  const signal = useData(key, fn, ttl);
  if (entry) entry.refetch = fn;
  return signal;
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
  const repo = `${owner}/${name}`;
  const [getResolve] = useDataRefetchable(
    `resolve:${repo}/${rest}`,
    () => repos.repo(repo).resolve(rest),
    RESOLVE_TTL
  );
  const [getOut, setOut] = createSignal(undefined);
  createEffect(() => {
    const r = getResolve();
    if (!r || !r.sha) return setOut(undefined);
    const key = `sha:${r.sha}:${kind}:${r.path ?? ""}`;
    const [getSha] = useDataRefetchable(key, shaFetcher(owner, name, kind, r), SHA_TTL);
    setOut(getSha());
  });
  return [getOut, setOut]; // same [get] contract as useData
}
