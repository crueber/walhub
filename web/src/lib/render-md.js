// web/src/lib/render-md.js — the single markdown pipeline (D-WEB-7): marked
// (GFM) → DOMPurify → trusted HTML string for innerHTML. The sanitizer is the
// only innerHTML gate: marked passes raw HTML (incl. <script>) and
// javascript: URIs straight through, so renderBody MUST NOT be bypassed.
// DOMPurify needs a DOM, so this module exposes two entry points:
//   renderMarkdownHtml(src) — marked layer only; Node-importable, covered by
//     node --test (headless-testable logic per D-WEB-4).
//   renderBody(src) — full pipeline; browser-only, throws without a DOM.
//     Covered by the real-Chromium pass (task-list/table comment, dark+light,
//     zero console errors), not by node --test.
// Importable in Node: the DOMPurify default export is a factory there (no
// .sanitize), so the static import never touches the DOM at module scope.
import { marked } from "marked";
import DOMPurify from "dompurify";

// breaks:true preserves the markdown-lite paragraph contract (continuation
// lines join with <br>) that blob-md.test.js pins — a conscious deviation
// from the GFM default (breaks:false) and the §8 sketch. Fenced-code language
// keeps marked's standard class="language-<lang>" shape: nothing consumed the
// old data-lang attribute (no CSS/JS selector referenced it), and the class
// shape is what highlighters interoperate with.
marked.setOptions({ gfm: true, breaks: true });

// Pinned allowlists (explicit, not DOMPurify defaults floating across
// upgrades): the old sanitizer's tag/attribute set plus the GFM shapes marked
// adds — del/s (strikethrough), input + checked/disabled/type (task-list
// checkboxes), class (language-* code classes). URI gating (javascript:/data:
// dropped; http/https/mailto/relative kept) rides DOMPurify's default
// ALLOWED_URI_REGEXP. FORBID_CONTENTS drops the old DROP_CONTENT set
// (script/style/…) WITH their text content.
export const PURIFY_CONFIG = {
  ALLOWED_TAGS: [
    "p", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "a", "code",
    "pre", "em", "strong", "blockquote", "table", "thead", "tbody", "tr",
    "th", "td", "hr", "br", "img", "span", "del", "s", "input",
  ],
  ALLOWED_ATTR: [
    "href", "src", "alt", "title", "checked", "disabled", "type", "class",
  ],
  FORBID_TAGS: ["script", "style", "iframe", "object", "embed", "noscript"],
  FORBID_CONTENTS: ["script", "style", "iframe", "object", "embed", "noscript"],
};

const hasDOM = typeof window !== "undefined" && typeof window.document !== "undefined";
const purify = hasDOM && DOMPurify && typeof DOMPurify.sanitize === "function" ? DOMPurify : null;

/** renderMarkdownHtml(src) → unsanitized HTML string (marked GFM layer). Never trusted raw. */
export function renderMarkdownHtml(src) {
  return marked.parse(String(src ?? ""));
}

/** renderBody(src) → HTML safe for innerHTML (marked + pinned DOMPurify). Browser-only. */
export function renderBody(src) {
  if (!purify) throw new Error("renderBody requires a DOM (DOMPurify); use renderMarkdownHtml in Node");
  return purify.sanitize(renderMarkdownHtml(src), PURIFY_CONFIG);
}
