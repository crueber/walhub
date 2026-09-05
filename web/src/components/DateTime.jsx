// web/src/components/DateTime.jsx — issue #133: the ONE date renderer.
// Every timestamp in the UI goes through here: visible text is the tiered
// `fmtDate` (relative → relative+day → absolute), hover `title` is the
// user's-local wall time from `fmtDateTitle`. Pure text — dark/light themes
// unaffected. Falsy values render `fallback` (default "") with no <time>.
//
// props: { value, fallback? }

import { Show } from "solid-js";
import { fmtDate, fmtDateTitle } from "../lib/format.js";

export default function DateTime(props) {
  return (
    <Show when={props.value} fallback={props.fallback ?? ""}>
      {(v) => <time dateTime={String(v())} title={fmtDateTitle(v())}>{fmtDate(v())}</time>}
    </Show>
  );
}
