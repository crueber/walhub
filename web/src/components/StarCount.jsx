// web/src/components/StarCount.jsx — per-repo star count for repo listings
// (issue #137): renders `walhub (3 ⭐)`-style next to a repo link.
//
// Performance shape (documented, deliberate): one `GET …/api/social` per
// rendered row, keyed `social:{o}/{r}` through the shared `useData`
// promise-cache (single-flighted per key, `SOCIAL_TTL` 30 s, LRU-capped
// per 12 §2.4) — the owners page, the `/:owner` page, and the repo chrome
// all share fetches for the same repo, and repeat visits within the TTL
// cost zero GETs. The link itself renders immediately; the count appears
// after load behind a muted `(…)` placeholder, so counts never block page
// render. Worst case stays bounded by the #117 caps (50 owners × 10 repos
// = 500 GETs on a cold cache, each independent so one slow repo never
// blocks the rest); failures follow the data-layer contract (tray, not
// the page). No new endpoint, no new SDK method (`repo.social.get()`,
// 07 §§4–7).

import repos from "../../sdk/src/index.js";
import { Show } from "solid-js";
import { useData } from "../lib/data.js";
import { SOCIAL_TTL, fmtStars } from "../lib/stars.js";

/** <StarCount full="owner/name" /> — quiet muted count beside a repo link. */
export default function StarCount(props) {
  const full = () => props.full;
  const [getSocial] = useData(
    () => `social:${full()}`,
    () => repos.repo(full()).social.get(),
    SOCIAL_TTL,
  );
  return (
    <Show when={getSocial()} fallback={<span class="muted text-xs" aria-hidden="true">(…)</span>}>
      {(s) => {
        const t = fmtStars(s().stars);
        return t ? <span class="muted text-xs">{t}</span> : null;
      }}
    </Show>
  );
}
