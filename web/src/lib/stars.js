// web/src/lib/stars.js — star-count display for repo listings (issue #137).
// Headless-testable: no Solid, no DOM — importable in Node.

/**
 * Cache TTL for per-repo social counters (`GET …/api/social`, 07 §7:
 * SWR 60 s + ETag on the wire). Matches the `social:{o}/{r}` 30 s row in
 * the 08 §6 cache-key table — the `<StarCount>` component keys its
 * `useData` entry exactly that way, so the owners page, the `/:owner`
 * page, and any later listing share fetches for the same repo.
 */
export const SOCIAL_TTL = 30_000;

/**
 * fmtStars(n) → "(3 ⭐)"-style. Finite non-negative counts render;
 * anything else (null/undefined/NaN/negative — not-yet-loaded or bad
 * data) renders as "" so the call site shows its placeholder instead.
 */
export function fmtStars(n) {
  if (n === null || n === undefined || n === "") return "";
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return "";
  return `(${Math.floor(v)} ⭐)`;
}
