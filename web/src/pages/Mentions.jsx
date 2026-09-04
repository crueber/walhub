// web/src/pages/Mentions.jsx — advisory @mention suggestions (06 §7):
// a datalist of `@principal` options drawn from thread participants.
// Purely advisory — the server re-parses every body at write time (§3).

import { For } from "solid-js";

/**
 * @param {{id: string, names?: string[]}} props datalist id + principals
 */
export function MentionDatalist(props) {
  const names = () => [...new Set((props.names ?? []).filter(Boolean))].sort().slice(0, 50);
  return (
    <datalist id={props.id}>
      <For each={names()}>{(n) => <option value={`@${n}`} />}</For>
    </datalist>
  );
}
