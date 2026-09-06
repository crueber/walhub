// web/src/lib/render-md.js — the single markdown pipeline (D-WEB-7): marked
// (GFM) → DOMPurify → trusted HTML string for innerHTML. The sanitizer is the
// only innerHTML gate: marked passes raw HTML (incl. <script>) and
// javascript: URIs straight through, so renderBody MUST NOT be bypassed.
// DOMPurify needs a DOM, so this module exposes two entry points:
//   renderMarkdownHtml(src, ctx?) — marked layer + relative-URL resolution;
//     Node-importable, covered by node --test (headless-testable logic per
//     D-WEB-4).
//   renderBody(src, ctx?) — full pipeline; browser-only, throws without a DOM.
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

// --- relative-URL resolution (issue #182) --------------------------------------
// File prose (README tabs, blob preview, release notes) emits src/href
// verbatim; a relative reference would resolve against the SPA route URL and
// 404. resolveMarkdownUrls rewrites relative references at render time
// against the file's own coordinates:
//
//   ctx = { owner, repo, ref, dir }
//     owner/repo/ref — the repo plus the page's display short-ref (the same
//       string the page puts in its own tree/blob links; the resolver treats
//       a missing/empty ref as "no coordinates" and returns the HTML
//       unchanged).
//     dir — the file's directory, "" for the repo root.
//
//   images (img src) ........ → the raw-bytes endpoint at the same ref
//     (/{o}/{r}/api/blob/{ref}/{path}?raw — 07_api.md §9.5, the same shape
//     RepoClient.urls.raw builds; click-to-full-size keeps working).
//   links to *.md/*.markdown → /{o}/{r}/blob/{ref}/{path} (the in-app blob
//     view, consistent with the doctabs feature).
//   other relative links .... → the raw-bytes endpoint (bytes, not the blob
//     view: a non-renderable target must not land on the 2 MiB render-cap
//     page — documented trade-off). A source ?query gains &raw, a bare
//     source gains ?raw, a #frag-only suffix keeps ?raw ahead of the frag.
//   anchors (#frag), scheme URLs (http/https/mailto/… — and javascript:/data:),
//   protocol-relative (//host) → untouched. Dangerous schemes pass through
//   byte-identical so the DOMPurify gate still sees and drops them: the
//   resolver MUST NOT launder them into same-origin URLs (pinned by test).
//   A leading "/" is repo-root-relative (GitHub convention), otherwise the
//   reference resolves against dir. "." / ".." / duplicate slashes normalize,
//   ".." past the root clamps at the root (never escapes the repo), and
//   nothing is decoded or re-encoded — "%2e" never becomes ".." and "%20"
//   survives verbatim (traversal-safe by construction). ?query / #frag
//   suffixes are preserved verbatim.
//   Bodies without file coordinates (issue/PR threads, unsent previews) pass
//   no ctx → returned unchanged; a thread comment's relative URL has no repo
//   file to resolve against. Release notes pass { ref: tag, dir: "" }: the
//   tag names the ref and the root is the base.
//
// Pure string post-processing over marked's HTML (marked emits href="…" /
// src="…" double-quoted; author raw-HTML blocks with single quotes are
// covered too). Node-safe, so the matrix is unit-tested headless; renderBody
// applies it BEFORE the DOMPurify gate, and rewritten root-relative URLs ride
// the gate's relative-URL allowance (relative URLs becoming absolute
// same-origin is fine).

const MD_LINK_RE = /(<a\b[^>]*?\shref\s*=\s*)("[^"]*"|'[^']*')/gi;
const MD_IMG_RE = /(<img\b[^>]*?\ssrc\s*=\s*)("[^"]*"|'[^']*')/gi;
const SCHEME_RE = /^[A-Za-z][A-Za-z0-9+.-]*:/;
const MD_EXT_RE = /\.(?:md|markdown)$/i;

function splitUrlSuffix(url) {
  const i = url.search(/[?#]/);
  return i === -1 ? [url, ""] : [url.slice(0, i), url.slice(i)];
}

// The raw-bytes endpoint shape mirrors RepoClient.urls.raw (sdk/src/repo.js):
// same-origin here (client base ""), so the /api/blob/{ref}/{path}?raw shape
// is spelled out once in each place and pinned by test on both sides.
function rawBase(ctx) {
  return `/${ctx.owner}/${ctx.repo}/api/blob/${ctx.ref}`;
}

// Merge the ?raw marker with a preserved source suffix: a source query gains
// &raw, anything else (?raw ahead of a #frag, or a bare ?raw).
function withRaw(suffix) {
  const h = suffix.indexOf("#");
  const q = h === -1 ? suffix : suffix.slice(0, h);
  const f = h === -1 ? "" : suffix.slice(h);
  return q ? `${q}&raw${f}` : `?raw${f}`;
}

function normalizeRepoPath(path, dir) {
  const out = path.startsWith("/") ? [] : String(dir ?? "").split("/").filter(Boolean);
  for (const seg of String(path).split("/")) {
    if (!seg || seg === ".") continue;
    if (seg === "..") {
      if (out.length) out.pop(); // clamp at the root: never escapes the repo
      continue;
    }
    out.push(seg);
  }
  return out.join("/");
}

function rewriteUrl(url, ctx, isImage) {
  if (!url || url.startsWith("#") || url.startsWith("//") || SCHEME_RE.test(url)) return url;
  const [path, suffix] = splitUrlSuffix(url);
  const resolved = normalizeRepoPath(path, ctx.dir);
  if (isImage) return `${rawBase(ctx)}/${resolved}${withRaw(suffix)}`;
  const leaf = resolved.split("/").pop() ?? "";
  if (MD_EXT_RE.test(leaf)) return `/${ctx.owner}/${ctx.repo}/blob/${ctx.ref}/${resolved}${suffix}`;
  return `${rawBase(ctx)}/${resolved}${withRaw(suffix)}`;
}

function rewriteAttr(html, re, ctx, isImage) {
  return String(html).replace(re, (m, pre, quoted) => {
    const url = quoted.slice(1, -1);
    const next = rewriteUrl(url, ctx, isImage);
    return next === url ? m : `${pre}${quoted[0]}${next}${quoted[0]}`;
  });
}

/** resolveMarkdownUrls(html, ctx) → HTML with relative src/href resolved (pure, Node-safe). */
export function resolveMarkdownUrls(html, ctx) {
  if (html == null) return html;
  if (!ctx || !ctx.owner || !ctx.repo || !ctx.ref) return String(html);
  return rewriteAttr(rewriteAttr(html, MD_IMG_RE, ctx, true), MD_LINK_RE, ctx, false);
}

/** renderMarkdownHtml(src, ctx?) → unsanitized HTML string (marked GFM layer). Never trusted raw. */
export function renderMarkdownHtml(src, ctx) {
  return resolveMarkdownUrls(marked.parse(String(src ?? "")), ctx);
}

/** renderBody(src, ctx?) → HTML safe for innerHTML (marked + resolve + pinned DOMPurify). Browser-only. */
export function renderBody(src, ctx) {
  if (!purify) throw new Error("renderBody requires a DOM (DOMPurify); use renderMarkdownHtml in Node");
  return purify.sanitize(renderMarkdownHtml(src, ctx), PURIFY_CONFIG);
}
