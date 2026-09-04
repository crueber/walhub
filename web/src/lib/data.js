// web/src/lib/data.js — the §2.4 data layer: promise-cache with TTL revalidation,
// sha-addressed forever-cache, LRU cap, global error tray state and top-progress
// counter. Solid signals power it; App.jsx wires the progress bar + tray effects.
// D-WEB-6 (2026-09-02): same API on Solid primitives after the vanilla-ESM port.

import { createSignal, createEffect } from "solid-js";
import { TTL, collabKeys } from "./collab.js";
import { NOT_MODIFIED } from "../../sdk/src/errors.js";

/** 08 §6 TTL table (re-exported from the pure collab module). */
export { TTL };

export const DEFAULT_TTL = 5_000; // revalidation window for ref-dependent data
const MAX_ENTRIES = 400; // LRU cap
const TRAY_MAX = 6; // error tray entries
const TRAY_FADE_MS = 10_000; // auto-fade

export const REPO_TTL = 5_000; // repo context per §2.6
export const RESOLVE_TTL = 5_000; // resolve step of §9.2
export const SHA_TTL = Infinity; // sha-addressed payloads are immutable
export { TTL as TTL_TABLE } from "./collab.js"; // alias for the 08 §6 table

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

function start(key, entry, fn, { keepRefetch = false } = {}) {
  // Ordering guard (#41): every fetch is a generation. A body (or error)
  // commits only while its generation is still the newest — a stale
  // disk-cache hit or a reordered response resolving after a newer fetch
  // started is dropped and can never overwrite fresh state.
  if (!keepRefetch) entry.refetch = fn;
  const generation = ++entry.seq;
  const task = (async () => {
    try {
      const value = await fn();
      if (generation !== entry.seq) return; // stale loser: drop silently
      if (value === NOT_MODIFIED) return void (entry.at = Date.now()); // 304: silent keep-current
      entry.value = value;
      entry.at = Date.now();
      entry.error = null;
      entry.signal[1](value);
    } catch (err) {
      if (generation !== entry.seq) return; // stale error: never clobber, never tray-spam
      entry.error = err;
      entry.at = Date.now();
      reportError(err, key); // errors go to the tray, not into the page
    } finally {
      if (entry.promise === guarded) entry.promise = null;
    }
  })();
  const guarded = trackPending(task);
  entry.promise = guarded;
}

function ensureEntry(key) {
  let entry = cache.get(key);
  if (!entry) {
    entry = { signal: createSignal(undefined), promise: null, value: undefined, at: 0, error: null, refetch: null, seq: 0 };
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
 * Background refresh keeps stale data on screen; failures go to the tray.
 * The key may be a GETTER (reactive): when it changes, the effect re-points
 * at the new entry and copies its value into this hook's own signal —
 * same-route param changes never go stale, and consumers track ONE signal.
 */
export function useData(key, fn, ttl = DEFAULT_TTL) {
  const keyOf = () => (typeof key === "function" ? key() : key);
  const [getValue, setValue] = createSignal(undefined);
  createEffect(() => {
    const k = keyOf();
    const entry = ensureEntry(k);
    startIfStale(k, entry, fn, ttl);
    setValue(entry.signal[0]());
  });
  return [getValue];
}

// --- headless seam (tests + cache warmers) -----------------------------------
// useData's effect cannot run under Node (the solid-js server build has no
// effects), so headless tests drive the cache through prefetchData: the same
// ensureEntry → touch/evict → start path the effect uses, minus reactivity.

/**
 * Internal: fetch NOW for key (a new generation), return the entry's value
 * getter. Headless tests and cache warmers use this; components use useData.
 */
export function prefetchData(key, fn) {
  const entry = ensureEntry(key);
  touch(key, entry);
  evict();
  start(key, entry, fn);
  return entry.signal[0];
}

/** Normalize an API field that may be a list, a comma string, a bool, or
 * null into a displayable array — the describe/settings payloads mix all of
 * these (e.g. this_host.serves is a BOOLEAN, roles a list). */
export function asList(v) {
  if (Array.isArray(v)) return v;
  if (v === true) return ["yes"];
  if (v === false || v == null || v === "") return [];
  return String(v).split(",").map((x) => x.trim()).filter(Boolean);
}

/**
 * Force a refetch of one key (e.g. after a mutating call). ALWAYS starts a
 * new generation — even over an in-flight fetch: the ordering guard drops
 * whichever settles stale, so a mutation refetch colliding with an in-flight
 * (SSE/coalesced) read can no longer be skipped into showing old data. The
 * refetch runs under the SDK's HTTP-cache bypass, so it always reads a
 * post-mutation body, never a pre-mutation disk-cache hit (#41). The stored
 * refetch closure is kept as-is (no wrapper layering).
 */
export function invalidate(key) {
  const entry = cache.get(key);
  if (!entry || typeof entry.refetch !== "function") return;
  const refetch = entry.refetch;
  const run = () => refetch();
  const fn = repos && typeof repos.withNoStore === "function" ? () => repos.withNoStore(run) : run;
  start(key, entry, fn, { keepRefetch: true });
}

/** Force a refetch of every settled key under a prefix (list windows). */
export function invalidatePrefix(prefix) {
  const keys = [];
  for (const key of cache.keys()) {
    if (key.startsWith(prefix)) keys.push(key);
  }
  for (const key of keys) invalidate(key);
}

// --- 08 §4 invalidation-storm coalescing ------------------------------------
// A burst of collab frames (CI posting 30 check runs) MUST coalesce:
// keys are collected into a set and invalidated once per tick. Background
// reads still single-flight per key (startIfStale joins the in-flight
// fetch); invalidations always start a new generation and the ordering
// guard in start() drops whichever settles stale (#41).
const pendingInvalidations = new Set();
let invalidateScheduled = false;

/** Queue one key for coalesced invalidation (flushed once per tick). */
export function scheduleInvalidate(key) {
  pendingInvalidations.add(key);
  if (invalidateScheduled) return;
  invalidateScheduled = true;
  queueMicrotask(() => {
    invalidateScheduled = false;
    const keys = [...pendingInvalidations];
    pendingInvalidations.clear();
    for (const k of keys) {
      if (k.endsWith("*")) invalidatePrefix(k.slice(0, -1));
      else invalidate(k);
    }
  });
}

/**
 * invalidateCollab(full, frame) — the 08 §4 frame table: one repo stream
 * frame fans out to the data-layer keys it invalidates. Timelines append
 * via (num, seq) dedup at the component level; lists and headers refetch
 * here, coalesced. `full` is "owner/name".
 */
export function invalidateCollab(full, frame) {
  for (const key of collabKeys(full, frame)) scheduleInvalidate(key);
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
