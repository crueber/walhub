// web/src/components/ActivityStamp.jsx — per-repo last-active stamp for repo
// listings (issue #142): renders `active <DateTime>`-style next to a repo
// link, using the app-wide <DateTime> from #133 (relative text + local-tz
// hover title).
//
// SOURCE DECISION (documented per AGENTS.md law 12): the stamp is the latest
// COMMIT date via `GET …/commits?n=1` — the ref defaults to HEAD server-side
// (07 §9.6), so this costs ONE GET per repo with no summary fetch first for
// the head sha. Alternatives rejected: the summary (`GET …/api`, §9.1)
// carries NO date field at all, so it cannot source a stamp without a new
// backend field; the overview's `manifest.last_push` IS a date but it is
// push-time (not commit-time) AND the overview is a no-store heavyweight
// (health/bundles/plan recomputed per call) — more cost per row for a less
// precise signal. Honest-proxy note: a push that adds no commits (branch
// delete, tag-only push) does not move the stamp; "last commit" is the
// documented meaning, hence the `active` label. Empty repos render
// "no commits yet" (not a fake epoch).
//
// Performance shape (mirrors <StarCount>, issue #137): one GET per rendered
// row, keyed `activity:{o}/{r}` through the shared `useData` promise-cache
// (single-flighted per key, `ACTIVITY_TTL` 30 s, LRU-capped per 12 §2.4) —
// the owners page and the `/:owner` page share fetches, and repeat visits
// within the TTL cost zero GETs while entries survive the cap. The link
// renders first behind a muted `(…)` placeholder, so stamps never block
// first paint. Worst case stays bounded by the #117 caps (50 owners ×
// 10 repos = 500 GETs on a cold cache, each independent so one slow repo
// never blocks the rest); failures follow the data-layer contract (tray,
// not the page) — except the empty-repo 404 (unborn HEAD), which the fetch
// maps to `{commits: []}` so the row renders "no commits yet" with no tray
// spam. No new endpoint, no new SDK method (`repo.commits()`,
// 07 §9.6).

import repos from "../../sdk/src/index.js";
import { Show } from "solid-js";
import { useData } from "../lib/data.js";
import { ACTIVITY_TTL, latestActivity } from "../lib/activity.js";
import DateTime from "./DateTime.jsx";

/** <ActivityStamp full="owner/name" /> — quiet muted stamp beside a repo link. */
export default function ActivityStamp(props) {
  const full = () => props.full;
  const [getPage] = useData(
    () => `activity:${full()}`,
    () =>
      repos.repo(full()).commits({ n: 1 }).catch((err) => {
        // Empty repo: unborn HEAD answers 404 — that IS the empty state
        // ("no commits yet"), not a failure, so it must not reach the
        // error tray. (A repo deleted between listing and fetch also 404s;
        // the row vanishes on the next list refresh.)
        if (err?.notFound) return { commits: [] };
        throw err;
      }),
    ACTIVITY_TTL,
  );
  return (
    <Show when={getPage()} fallback={<span class="muted text-xs" aria-hidden="true">(…)</span>}>
      {(page) => {
        const at = latestActivity(page());
        return at ? (
          <span class="muted whitespace-nowrap text-xs">
            active <DateTime value={at} />
          </span>
        ) : (
          <span class="muted text-xs">no commits yet</span>
        );
      }}
    </Show>
  );
}
