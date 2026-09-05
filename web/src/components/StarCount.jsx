// web/src/components/StarCount.jsx — per-repo star count for repo listings
// (issue #137): renders `walhub (3 ⭐)`-style next to a repo link.
//
// Performance shape (documented, deliberate): one `GET …/api/social` per
// rendered row, keyed `social:{o}/{r}` through the shared `useData`
// promise-cache (single-flighted per key, `SOCIAL_TTL` 30 s, LRU-capped
// per 12 §2.4) — the owners page and the `/:owner` page share fetches for
// the same repo (the repo-chrome star toggle calls the same SDK method
// directly, outside this cache), and repeat visits within the TTL cost
// after load behind a muted `(…)` placeholder, so counts never block page
// render. Worst case stays bounded by the #117 caps (50 owners × 10 repos
// = 500 GETs on a cold cache, each independent so one slow repo never
// blocks the rest). Failures follow the data-layer contract (tray, not the
// page) — EXCEPT 404, which is an expected listing state, not an error
// (deleted/private, or a fork-provisioned prefix whose child manifest does
// not exist yet — issue #150): the fetch resolves to null via
// `tolerateMissing` and the row hides the count silently, never traying.
// No new endpoint, no new SDK method (`repo.social.get()`,
// 07 §§4–7).

import repos from "../../sdk/src/index.js";
import { Show } from "solid-js";
import { useData, tolerateMissing } from "../lib/data.js";
import { SOCIAL_TTL, fmtStars } from "../lib/stars.js";

/** <StarCount full="owner/name" /> — quiet muted count beside a repo link. */
export default function StarCount(props) {
  const full = () => props.full;
  const [getSocial] = useData(
    () => `social:${full()}`,
    () => tolerateMissing(repos.repo(full()).social.get(), null),
    SOCIAL_TTL,
  );
  // undefined = still loading → placeholder; null = missing (#150) → hidden.
  return (
    <Show when={getSocial() !== undefined} fallback={<span class="muted text-xs" aria-hidden="true">(…)</span>}>
      <Show when={getSocial()}>
        {(s) => {
          const t = fmtStars(s().stars);
          return t ? <span class="muted text-xs">{t}</span> : null;
        }}
      </Show>
    </Show>
  );
}
