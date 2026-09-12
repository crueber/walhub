/**
 * Shared access-save path (Forgejo #391): the visibility-only save both
 * the Settings general tab and (for wording) the Access tab use.
 *
 * Two tabs used to save the same `access.json` doc through two
 * hand-rolled flows that drifted: Settings kept the user's chosen value
 * in the select after ANY failure (403/409/network), so a refresh
 * reseeding from server truth looked like the save "not sticking"
 * (the bounce). Every failure here is authoritative + loud: the caller
 * reseeds the select from server truth and notes the specific cause.
 *
 * saveVisibilityOnly takes the CAS version from a FRESH access.get()
 * immediately before each PUT (never the 5 s useData entry) and retries
 * ONCE on 409 with a re-read version — the Access-tab pattern. The
 * retry is safe only because each attempt re-reads the bindings it
 * preserves; the Access tab's full-document binding edit intentionally
 * does NOT auto-retry (a blind retry would clobber a concurrent
 * binding edit — there 409 means reload, as before).
 */

import { isVisibility } from "./visibility.js";

/**
 * friendlyAccessError(err) → the specific note the UI shows for an
 * access-save failure. Shared so both tabs word the same failure the
 * same way (previously duplicated in Access.jsx).
 */
export function friendlyAccessError(err) {
  if (!err) return "";
  if (err.status === 409) return "changed under you — reloaded the latest version";
  if (err.status === 403) return "admin role required to change access";
  if (err.status === 401) return "sign in to view access";
  return String(err.message ?? err);
}

/**
 * saveVisibilityOnly(repo, vis) → the PUT's authoritative echo.
 * Fresh GET → PUT (version + current bindings preserved, visibility
 * flipped) → on 409, one retry on a freshly re-read version. Throws
 * the original SDK error (status preserved) when out of retries.
 */
export async function saveVisibilityOnly(repo, vis) {
  if (!isVisibility(vis)) {
    throw new Error("visibility must be public, authenticated, or private");
  }
  const attempt = async () => {
    const doc = await repo.access.get();
    return repo.access.put({
      version: doc?.version ?? 0,
      visibility: vis,
      role_bindings: doc?.role_bindings ?? [],
    });
  };
  try {
    return await attempt();
  } catch (err) {
    if (err?.status !== 409) throw err;
    return await attempt();
  }
}

/**
 * reseedVisibility(repo) → the server's current visibility spelling,
 * or null when unreadable (caller keeps its current value then — a
 * failed reseed must never blank the select).
 */
export async function reseedVisibility(repo) {
  try {
    const doc = await repo.access.get();
    return isVisibility(doc?.visibility) ? doc.visibility : null;
  } catch {
    return null;
  }
}
