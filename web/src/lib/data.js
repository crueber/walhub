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

// --- expected-404 auxiliary fetches (issue #150) -------------------------------
// A per-row auxiliary fetch (star counts, last-active stamps) 404s in
// EXPECTED states: the row's repo was deleted between listing and fetch, is
// invisible to this viewer, or is a provisioned-but-unborn prefix — a fork
// writes repos/<o>/<r>/fork.json before the child manifest exists, so the
// prefix listing names the child while every manifest-gated read 404s. A
// 404 there is missing DATA, not a failure: the tray is for real errors.
// Wrap the fetcher so the 404 resolves to `missing` (the row renders its
// placeholder/hidden state); any other error still throws into the tray
// path unchanged.

/**
 * tolerateMissing(promise, missing) → resolves `missing` when the fetch
 * answers SDK 404 (`err.notFound`); rethrows everything else untouched.
 */
export function tolerateMissing(promise, missing) {
  return Promise.resolve(promise).catch((err) => {
    if (err?.notFound) return missing;
    throw err;
  });
}

// --- empty / degraded repo states (issue #209) --------------------------------
// An empty repo (manifest present, zero refs) is a legitimate state, not
// damage: the Code tab guides instead of fetching, and the shell never
// toasts. A degraded repo (refs present, cached fsck.pb lists missing
// objects) renders inline notices + an amber banner, never toasts.

/** Server 404 prefix for unborn-repo reads (07_api.md §9.9). */
export const EMPTY_MARKER = "empty repository: ";

/** Sentinel: the repo is known-empty — render the guide, fetch nothing. */
export const EMPTY_REPO = { empty: true };

/** Sentinel: the object is missing on a known-degraded repo — inline notice. */
export const DEGRADED_MISSING = { degraded: true };

/**
 * isEmptySummary(s) — the EmptyRepoGuide predicate: the server's
 * `health:"empty"` when present, else the unborn shape (`!head` + zero
 * branches/tags) so old servers still guide. Never true for null (deleted
 * repos keep the "not found" shell, never the guide).
 */
export function isEmptySummary(s) {
  if (!s) return false;
  if (s.health === "empty") return true;
  if (s.health === "healthy" || s.health === "degraded") return false;
  return !s.head && !s.branches && !s.tags;
}

/** isDegradedSummary(s) — the amber-banner / inline-notice predicate. */
export function isDegradedSummary(s) {
  return !!s && s.health === "degraded";
}

/**
 * peekCached(key) → the settled value or undefined (review S3: the named
 * mechanism for reading the shared `repo:{full}` summary entry without a
 * fetch). Non-reactive by design — call it inside effects/fetchers at
 * fetch time, never as a render source (render sources subscribe via
 * useData on the same key or via the repo context).
 */
export function peekCached(key) {
  return cache.get(key)?.value;
}

/** summaryOf(full) — peek the shell's shared summary entry for a repo. */
export function summaryOf(full) {
  return peekCached(`repo:${full}`);
}

/**
 * isEmptyError(err) — the self-describing fallback: a 404 carrying the
 * server's empty marker maps to silent + guide even when the summary is not
 * yet loaded. Damage-404s carry no marker by design (frozen wire), so they
 * never match — degraded-as-empty misclassification is impossible here.
 */
export function isEmptyError(err) {
  return !!err?.notFound && String(err?.message ?? "").includes(EMPTY_MARKER);
}

/**
 * tolerateDegraded(promise, full, missing) → resolves `missing` when the
 * fetch 404s AND the shell already knows the repo is degraded (read via
 * peek — no fetch); rethrows everything else untouched. The tolerateMissing
 * discipline extended: expected control flow ≠ reportError. Never mutes 404s
 * globally — without a known-degraded summary every 404 still throws into
 * the tray path as before.
 */
export function tolerateDegraded(promise, full, missing) {
  return Promise.resolve(promise).catch((err) => {
    if (err?.notFound && isDegradedSummary(summaryOf(full))) return missing;
    throw err;
  });
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
  entry.lastStart = Date.now(); // Forgejo #396: fetch-start stamp for the SSE fresh-skip below
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
    entry = { signal: createSignal(undefined), promise: null, value: undefined, at: 0, error: null, refetch: null, seq: 0, lastStart: 0 };
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

/**
 * patchCached(key, fn) → boolean: the generation-safe optimistic-commit
 * path (issues #36/#42). Maps the cached value through fn and commits the
 * result to subscribers synchronously, so a mutation paints immediately
 * instead of waiting for the guarded refetch round trip.
 *
 * Safety vs the #41 ordering guard: the entry generation is retired first
 * (entry.seq++), so any pre-mutation body still in flight resolves as a
 * stale loser and is dropped — the optimistic value can only be replaced
 * by a fetch that started after it. The caller's invalidate() then starts
 * a newer generation whose post-mutation body reconciles the guess; on
 * mutation failure the same invalidate rolls the guess back. fn MUST
 * return a new object (see reactions.adjustSummary) — Solid signals use
 * reference equality and an in-place mutation would not notify.
 *
 * ### Concurrency
 * Hazard: synchronous signal commit racing an in-flight fetch resolution.
 * Avoidance: generation retirement happens before the signal commit, so
 * the race resolves deterministically in favor of the optimistic value;
 * every flow that calls patchCached must follow with invalidate() (or a
 * compensating patchCached on error) so the guess is always reconciled.
 */
export function patchCached(key, fn) {
  const entry = cache.get(key);
  if (!entry || entry.value === undefined) return false;
  entry.seq++; // retire pre-mutation in-flight bodies (they resolve stale)
  entry.value = fn(entry.value);
  entry.at = Date.now();
  entry.signal[1](entry.value);
  return true;
}

/** Force a refetch of every settled key under a prefix (list windows). */
export function invalidatePrefix(prefix) {
  const keys = [];
  for (const key of cache.keys()) {
    if (key.startsWith(prefix)) keys.push(key);
  }
  for (const key of keys) invalidate(key);
}

/**
 * invalidateIssueLists(full) — the mutation-site reconcile for thread
 * mutations (issue #318). The promise cache is GLOBAL across Solid-router
 * navigations (one module-level Map), so a PATCH on the issue page must
 * invalidate the repo-wide list surfaces directly instead of relying on
 * the SSE round-trip through a page that may unmount mid-navigation:
 * every `issues:{full}:*` window (Issues.jsx query windows AND the
 * milestones page's `issues:{full}:milestone:{id}` entries) plus the
 * `milestones:{full}` counts. invalidate() on an uncached key is a
 * silent no-op, so calling this when no list page was ever mounted is
 * free. Callers still invalidate their own thread key separately.
 *
 * Issue #319: the shell's shared `repo:{full}` summary entry also
 * reconciles here — it carries the open_issues badge numerator, which a
 * close/reopen moves with no ref move.
 */
export function invalidateIssueLists(full) {
  invalidatePrefix(`issues:${full}:`);
  invalidate(`milestones:${full}`);
  invalidate(`repo:${full}`);
}

// --- 08 §4 invalidation-storm coalescing (Forgejo #396) ----------------------
// A burst of collab frames (CI posting 30 check runs) MUST coalesce:
// keys are collected into a set and invalidated once per tick. The tick
// alone does NOT bound live traffic — frames arriving in separate tasks
// each flushed a full refetch round per cached key (a 64-frame spread
// replay cost ~3 GETs/frame with zero TTL respect). The flush is therefore
// TTL-aware: an SSE invalidation for an already-fresh entry is a no-op,
// so sustained frame rates decay to the key's TTL cadence (5 s typical;
// Infinity = immutable windows, never SSE-refetched — timelines append
// frames directly, so skipping the refetch loses nothing). Mutation
// invalidations still go through invalidate() directly and ALWAYS refetch
// (#41 ordering: a post-mutation body must win over any in-flight read).
// scheduleInvalidate is the SSE path ONLY (sole caller: invalidateCollab).
const pendingInvalidations = new Set();
let invalidateScheduled = false;

/**
 * Fresh window (ms) for one SSE-invalidated key: the 08 §6 TTL table by
 * key prefix, DEFAULT_TTL for unlisted prefixes (settings:/mirror:/ops:/
 * overview:/org:… all revalidate at 5 s in their useData seeds).
 */
function ttlForKey(key) {
  const v = TTL[key.split(":")[0]];
  return v === undefined ? DEFAULT_TTL : v;
}

/** Queue one key for coalesced invalidation (flushed once per tick). */
export function scheduleInvalidate(key) {
  pendingInvalidations.add(key);
  if (invalidateScheduled) return;
  invalidateScheduled = true;
  queueMicrotask(() => {
    invalidateScheduled = false;
    const keys = [...pendingInvalidations];
    pendingInvalidations.clear();
    // Expand prefixes FIRST so each entry invalidates at most once per
    // flush: a sha key queued both explicitly and via its `*` prefix used
    // to start two generations (two GETs) for a single frame.
    const expanded = new Set();
    for (const k of keys) {
      if (k.endsWith("*")) {
        const base = k.slice(0, -1);
        for (const ck of cache.keys()) {
          if (ck.startsWith(base)) expanded.add(ck);
        }
      } else expanded.add(k);
    }
    const now = Date.now();
    for (const k of expanded) {
      const entry = cache.get(k);
      if (!entry || typeof entry.refetch !== "function") continue; // uncached = silent no-op
      const freshMs = ttlForKey(k);
      // Fresh entry, fresh news already on the way or just landed: skip.
      // In-flight counts by START time (a slow read older than the window
      // still gets superseded, as before); settled counts by settle time.
      if (entry.promise) {
        if (now - (entry.lastStart || 0) < freshMs) continue;
      } else if (entry.at > 0 && now - entry.at < freshMs) continue;
      invalidate(k);
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
 *
 * Issue #209 short-circuits: when the shell's shared summary entry already
 * knows the repo is empty, both steps are suppressed (the selArgs-null
 * pattern — a suppressed fetch, not a filtered error) and the hook settles
 * the EMPTY_REPO sentinel so pages render the guide with zero doomed
 * requests and zero toasts. Fallback: an empty-prefixed resolve 404 maps to
 * the same sentinel when the summary is not yet loaded. Degraded repos map
 * sha-step 404s to DEGRADED_MISSING for inline notices (healed objects need
 * an invalidate to clear the cached sentinel — pages offer a retry that
 * does exactly that).
 */
export function useResolved(owner, name, rest, kind) {
  const v = (x) => (typeof x === "function" ? x() : x);
  const [getOut, setOut] = createSignal(undefined);
  createEffect(() => {
    const o = v(owner), n = v(name), rv = v(rest) ?? "";
    const repo = `${o}/${n}`;
    // Step 0: known-empty suppression — no entry touched, no fetch started.
    if (isEmptySummary(summaryOf(repo))) return setOut(EMPTY_REPO);
    // Step 1: resolve (ref-dependent, 5s SWR).
    const rKey = `resolve:${repo}/${rv}`;
    const rEntry = ensureEntry(rKey);
    startIfStale(rKey, rEntry, () => repos.repo(repo).resolve(rv).catch((err) => {
      if (isEmptyError(err)) return { empty: true };
      throw err;
    }), RESOLVE_TTL);
    const r = rEntry.signal[0]();
    if (!r || !r.sha) {
      if (r?.empty) return setOut(EMPTY_REPO);
      return setOut(undefined);
    }
    // Step 2: sha-addressed payload (immutable).
    const sKey = `sha:${r.sha}:${kind}:${r.path ?? ""}`;
    const sEntry = ensureEntry(sKey);
    startIfStale(sKey, sEntry, () => shaFetcher(o, n, kind, r)().catch((err) => {
      if (err?.notFound && isDegradedSummary(summaryOf(repo))) {
        return { ...DEGRADED_MISSING, ref: r.ref, sha: r.sha, path: r.path ?? "" };
      }
      throw err;
    }), SHA_TTL);
    const out = sEntry.signal[0]();
    if (!out) return setOut(undefined);
    if (out.empty || out.degraded) return setOut(out);
    // Sha-addressed payloads are ref-free by design (§2.4); the UI builds
    // URLs from the ref the user resolved, so attach it here.
    setOut(out && !out.ref ? { ...out, ref: r.ref } : out);
  });
  return [getOut, setOut]; // same [get] contract as useData
}
