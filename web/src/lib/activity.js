// web/src/lib/activity.js — last-active stamp for repo listings (issue #142).
// Headless-testable: no Solid, no DOM — importable in Node.

/**
 * Cache TTL for the per-repo latest-commit fetch (`GET …/commits?n=1`, 07
 * §9.6: SWR on a HEAD ref). Mirrors `SOCIAL_TTL` (30 s): the
 * `<ActivityStamp>` component keys its `useData` entry `activity:{o}/{r}`,
 * so the owners page and the `/:owner` page share fetches for the same
 * repo. Same cost bound as star counts (capped lists, independent rows).
 */
export const ACTIVITY_TTL = 30_000;

/**
 * latestActivity(page) → ISO date string | null. Extracts the stamp from a
 * `GET …/commits?n=1` page: `commit_date` first (when the commit landed in
 * this repo's history — rebases/cherry-picks move it), `author_date` as
 * fallback (original authorship). Empty repos (no commits) and missing
 * pages yield null so the call site shows its "no commits yet" text.
 */
export function latestActivity(page) {
  const c = page?.commits?.[0];
  return c?.commit_date ?? c?.author_date ?? null;
}
