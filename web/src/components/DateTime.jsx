// web/src/components/DateTime.jsx — issue #133: the ONE date renderer.
// Every timestamp in the UI goes through here: visible text is the tiered
// `fmtDate` (relative → relative → absolute; issue #312 moved the middle
// tier's "{ordinal} of {Month}" into the hover title), hover `title` is the
// user's-local wall time from `fmtDateTitle` (calendar-date-prefixed in the
// 1–30-day window). Pure text — dark/light themes
// unaffected. Falsy values render `fallback` (default "") with no <time>.
//
// props: { value, fallback? }

import { Show } from "solid-js";
import { dateTimeAttr, fmtDate, fmtDateTitle } from "../lib/format.js";

export default function DateTime(props) {
  return (
    <Show when={props.value} fallback={props.fallback ?? ""}>
      {(v) => <time dateTime={dateTimeAttr(v())} title={fmtDateTitle(v())}>{fmtDate(v())}</time>}
    </Show>
  );
}
