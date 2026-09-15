// web/src/components/DiffTable.jsx — shared selectable diff renderer (issue #244).
//
// One DiffBody serves the commit page (Commit.jsx) and the PR files page
// (PullFiles.jsx): per-file unified/split tables with line-number gutters,
// click/drag/shift-click selection, and shareable file-scoped hashes
// (lib/diff-lines.js). Used in both modes with the SAME hash — a selection
// names a file line, not a view column.
//
// Selection model (Solid signals, no library — mirrors CodeLines in
// Blob.jsx, issue #243): mousedown on a gutter number sets the anchor,
// mouseover extends the focus (drag), mouseup ends the drag; the click then
// pushes ONE history entry. During the drag each frame uses replaceState
// (no history spam, URL still shareable mid-drag). Shift-click extends from
// the existing anchor. Cross-chunk drags clamp: mouseover in another hunk
// (or on the other side) is ignored, so the range stops at the chunk edge —
// GitHub's behavior. Code cells carry no handlers, so normal text selection
// in the code area is untouched.
//
// Numbering is display-only: gutter numbers derive from the @@ headers at
// render time. The drift-hash inputs (hunk.lines → anchorContextSha in
// lib/diff.js, twin DriftHash in internal/review/model.go) are never
// touched — the pinned vectors in diff-review.test.js are the tripwire.
//
// Commenting (Forgejo #546): pass onCommentSelect (+ canComment) and the
// current selection gains a "comment on selection" button feeding the
// consumer's shared CommentComposer. Anchor construction lives in
// lib/review-anchor.js — this component never hashes, never prompts.
//
// Row backgrounds (issue #544): lineClass() applies per CELL, not per row
// — unified rows color both cells (gutter + code, full-row background),
// split rows color per side (a paired change row is red-left/green-right,
// never one color across). The .diff-num.diff-add/.diff-del compound rules
// in ui.css beat the plain .diff-num background at any order; the
// unlayered .diff-row.line-hl selection rule still wins over all of them.

import { createSignal, createEffect, onCleanup, untrack, For, Show } from "solid-js";
import {
  annotateHunkLines,
  annotateSplitRows,
  unifiedNo,
  unifiedSide,
  matchAnnotated,
  diffLineHash,
  parseDiffHash,
  sameDiffSelection,
} from "../lib/diff-lines.js";
import { dragRange } from "../lib/blob-lines.js";

export const lineClass = (t) => (t === "+" ? "diff-add" : t === "-" ? "diff-del" : "");

const hunkHead = (h) => `@@ -${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines} @@ ${h.context ?? ""}`;

export function DiffBody(props) {
  const path = () => props.file?.path ?? "";
  const hunks = () => props.file?.hunks ?? [];
  const mode = () => props.mode ?? "unified";

  const readSel = () => {
    const s = parseDiffHash(window.location.hash);
    return s && s.path === path() ? s : null;
  };
  const [getSel, setSel] = createSignal(readSel());
  // Anchor: the hunk + side the drag started on, plus its line number.
  // (key is the row identity — unused beyond the hunk/side clamp.)
  let anchor = null;
  let dragging = false;

  const slug = () => path().replace(/[^a-zA-Z0-9_-]/g, "-");
  const uniId = (h, i) => `d-${slug()}-${h}-${i}`;
  const splitId = (h, j) => `ds-${slug()}-${h}-${j}`;

  // First highlighted row for scroll-into-view on shared-URL load.
  const firstMatchId = (s) => {
    const hs = hunks();
    if (mode() === "split") {
      for (let h = 0; h < hs.length; h++) {
        const rows = annotateSplitRows(hs[h]);
        for (let j = 0; j < rows.length; j++) {
          const cell = s.side === "old" ? rows[j].left : rows[j].right;
          if (cell?.no != null && cell.no >= s.start && cell.no <= s.end) return splitId(h, j);
        }
      }
      return null;
    }
    for (let h = 0; h < hs.length; h++) {
      const idxs = matchAnnotated(annotateHunkLines(hs[h]), s.side, s.start, s.end);
      if (idxs.length) return uniId(h, idxs[0]);
    }
    return null;
  };

  // Re-highlight + scroll once this file's rows exist. Tracks the file
  // data + mode only: the sel read + scroll MUST stay untracked, or every
  // drag frame would yank the scroll back to the anchor instead of
  // following the pointer (same trap as CodeLines in Blob.jsx). A hash
  // naming an unknown path or out-of-range line matches no row and scrolls
  // nowhere.
  createEffect(() => {
    const hs = hunks();
    const p = path();
    const m = mode();
    void m;
    if (!hs.length || !p) return;
    untrack(() => {
      const next = readSel();
      if (!sameDiffSelection(next, getSel())) {
        setSel(next);
        anchor = null;
      }
      if (next) {
        const id = firstMatchId(next);
        if (id) document.getElementById(id)?.scrollIntoView({ block: "center" });
      }
    });
  });

  const readHash = () => {
    const next = readSel();
    if (!sameDiffSelection(next, getSel())) {
      setSel(next);
      anchor = null;
    }
  };

  const show = (a, focusNo) => {
    const r = dragRange(a.no, focusNo);
    if (!r) return;
    const sel = { path: path(), side: a.side, start: r.start, end: r.end };
    setSel(sel);
    window.history.replaceState(null, "", diffLineHash(sel.path, sel.side, sel.start, sel.end));
  };

  const begin = (h, side, no, ev) => {
    if (ev.button !== 0 || no == null) return;
    anchor = ev.shiftKey && anchor && anchor.hunk === h && anchor.side === side ? anchor : { hunk: h, side, no };
    dragging = true;
    show(anchor, no);
    ev.preventDefault();
  };

  const extend = (h, side, no) => {
    if (!dragging || !anchor || no == null) return;
    if (h !== anchor.hunk || side !== anchor.side) return; // clamp to chunk + side
    show(anchor, no);
  };

  const endDrag = () => {
    dragging = false;
  };

  const activate = (h, side, no, ev) => {
    ev.preventDefault();
    // Mouse click lands after mousedown already previewed the range — push
    // that range as-is. Keyboard Enter/Space arrives as a click with
    // detail 0 and no drag before it: plain Enter jumps to the focused
    // line, Shift+Enter extends from the anchor (same split as CodeLines).
    if (ev.detail !== 0) {
      const s = getSel();
      if (s && s.path === path()) window.location.hash = diffLineHash(s.path, s.side, s.start, s.end);
      return;
    }
    if (no == null) return;
    const a = ev.shiftKey && anchor && anchor.hunk === h && anchor.side === side ? anchor : { hunk: h, side, no };
    anchor = a;
    const r = ev.shiftKey ? dragRange(a.no, no) : dragRange(no, no);
    if (!r) return;
    const sel = { path: path(), side: a.side, start: r.start, end: r.end };
    setSel(sel);
    window.location.hash = diffLineHash(sel.path, sel.side, sel.start, sel.end);
  };

  window.addEventListener("hashchange", readHash);
  window.addEventListener("mouseup", endDrag);
  window.addEventListener("blur", endDrag);
  onCleanup(() => {
    window.removeEventListener("hashchange", readHash);
    window.removeEventListener("mouseup", endDrag);
    window.removeEventListener("blur", endDrag);
  });

  const gutterLink = (h, side, no, label) => (
    <Show when={no != null} fallback={no ?? ""}>
      <a
        href={diffLineHash(path(), side, no, no)}
        aria-label={label}
        onMouseDown={(ev) => begin(h, side, no, ev)}
        onMouseOver={() => extend(h, side, no)}
        onClick={(ev) => activate(h, side, no, ev)}
      >
        {no}
      </a>
    </Show>
  );

  const inSel = (side, no) => {
    const s = getSel();
    return !!s && s.side === side && no != null && no >= s.start && no <= s.end;
  };

  return (
    <>
    <Show
      when={!props.file?.isBinary}
      fallback={
        <table class="diff w-full font-mono text-xs">
          <tbody>
            <tr>
              <td class="muted p-3" colspan={mode() === "split" ? 4 : 2}>
                Binary file not shown
              </td>
            </tr>
          </tbody>
        </table>
      }
    >
      <table class="diff w-full font-mono text-xs leading-5">
        <tbody>
          <Show
            when={mode() === "split"}
            fallback={
              <For each={hunks()}>
                {(h, hi) => (
                  <>
                    <tr>
                      <td class="diff-hunk px-3" colspan={2}>
                        {hunkHead(h)}
                      </td>
                    </tr>
                    <For each={annotateHunkLines(h)}>
                      {(l, li) => {
                        const no = unifiedNo(l);
                        const side = unifiedSide(l);
                        return (
                          <tr id={uniId(hi(), li())} class="diff-row" classList={{ "line-hl": inSel(side, no) }}>
                            <td class={`diff-num ${lineClass(l.t)}`}>
                              {gutterLink(hi(), side, no, `Diff line ${no}${side === "old" ? " (old side)" : ""} in ${path()}`)}
                            </td>
                            <td class={lineClass(l.t)}>{l.text || " "}</td>
                          </tr>
                        );
                      }}
                    </For>
                  </>
                )}
              </For>
            }
          >
            <For each={hunks()}>
              {(h, hi) => (
                <>
                  <tr>
                    <td class="diff-hunk px-3" colspan={4}>
                      {hunkHead(h)}
                    </td>
                  </tr>
                  <For each={annotateSplitRows(h)}>
                    {(row, ri) => (
                      <tr id={splitId(hi(), ri())} class="diff-row" classList={{ "line-hl": inSel("old", row.left?.no) || inSel("new", row.right?.no) }}>
                        <td class={`diff-num ${row.left ? lineClass(row.left.t) : ""}`}>
                          {gutterLink(hi(), "old", row.left?.no, `Diff line ${row.left?.no} (old side) in ${path()}`)}
                        </td>
                        <td class={row.left ? lineClass(row.left.t) : ""}>{row.left ? row.left.text || " " : ""}</td>
                        <td class={`diff-num ${row.right ? lineClass(row.right.t) : ""}`}>
                          {gutterLink(hi(), "new", row.right?.no, `Diff line ${row.right?.no} (new side) in ${path()}`)}
                        </td>
                        <td class={row.right ? lineClass(row.right.t) : ""}>{row.right ? row.right.text || " " : ""}</td>
                      </tr>
                    )}
                  </For>
                </>
              )}
            </For>
          </Show>
        </tbody>
      </table>
    </Show>
      {/* Comment-on-selection affordance (Forgejo #546): when a consumer
          passes onCommentSelect (+ canComment), the current drag/shift
          selection gains a "comment on selection" button feeding the shared
          CommentComposer on the consumer's side — never a prompt here. The
          selection stays clamped to one chunk + one side by the drag logic,
          which maps cleanly onto the §4 range anchor. Hidden everywhere
          else (commit page, ungated consumers pass nothing). */}
      <Show when={props.onCommentSelect && props.canComment !== false && getSel()}>
        {(sel) => (
          <div class="mt-1 flex flex-wrap items-center gap-2 text-xs">
            <span class="muted font-mono">
              {sel().path}:{sel().start}{sel().end !== sel().start ? `-${sel().end}` : ""}{sel().side === "old" ? " (old side)" : ""}
            </span>
            <button
              type="button"
              class="btn px-2 py-0.5"
              onClick={() => props.onCommentSelect(sel())}
              aria-label={`Comment on selected lines in ${sel().path}`}
            >
              comment on selection
            </button>
          </div>
        )}
      </Show>
    </>
  );
}
