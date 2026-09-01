// web/src/lib/sanitize.js — §2.2: ~40-line allowlist sanitizer for markdown preview
// output. Tags allowed: p, h1–h6, ul, ol, li, a, code, pre, em, strong, blockquote,
// table, thead, tbody, tr, th, td, hr, br, img, span. Attributes: href/src/alt/title
// only; href/src schemes restricted to http, https, mailto, /, # (relative paths
// without a colon are kept). Pure string ops — importable in Node.

const VOID = new Set(["br", "hr", "img"]);
const DROP_CONTENT = new Set(["script", "style", "iframe", "object", "embed", "noscript"]);
const TAGS = new Set([
  "p", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "a", "code", "pre", "em",
  "strong", "blockquote", "table", "thead", "tbody", "tr", "th", "td", "hr", "br", "img", "span",
]);
const ATTRS = {
  a: new Set(["href", "title"]),
  img: new Set(["src", "alt", "title"]),
};
const SAFE_SCHEME = /^(https?:|mailto:|\/|#)/i;
const TAG_RE = /<\/?([a-zA-Z][a-zA-Z0-9-]*)((?:\s[^<>]*)?)\/?>/g;
const ATTR_RE = /([a-zA-Z-]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>`]+))/g;

function esc(s) {
  return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

function safeUrl(v) {
  const url = String(v ?? "").trim();
  if (url === "") return null;
  if (SAFE_SCHEME.test(url)) return url;
  if (!url.includes(":")) return url; // scheme-less relative path
  return null; // javascript:, data:, vbscript:, … — dropped
}

function filterAttrs(name, raw) {
  const allowed = ATTRS[name];
  if (!allowed || raw === undefined || raw === "") return "";
  let out = "";
  for (const m of raw.matchAll(ATTR_RE)) {
    const attr = m[1].toLowerCase();
    if (!allowed.has(attr)) continue;
    const value = m[2] ?? m[3] ?? m[4] ?? "";
    if (attr === "href" || attr === "src") {
      const safe = safeUrl(value);
      if (safe === null) continue;
      out += ` ${attr}="${esc(safe)}"`;
    } else {
      out += ` ${attr}="${esc(value)}"`;
    }
  }
  return out;
}

/** sanitize(html) → HTML safe for innerHTML (allowlist of tags and attributes). */
export function sanitize(html) {
  let out = "";
  let last = 0;
  for (const m of String(html ?? "").matchAll(TAG_RE)) {
    out += esc(String(html).slice(last, m.index));
    last = m.index + m[0].length;
    const name = m[1].toLowerCase();
    const closing = m[0][1] === "/";
    const selfClosing = /\/>$/.test(m[0]);
    if (DROP_CONTENT.has(name)) {
      if (closing) continue; // its opener already consumed the whole span
      // drop the tag AND its text content until the matching close tag
      const rest = String(html).slice(last);
      const close = new RegExp(`</${name}\\s*>`, "i").exec(rest);
      last += close ? close.index + close[0].length : rest.length;
      continue;
    }
    if (!TAGS.has(name)) continue; // drop the tag, keep its inner text
    const attrs = filterAttrs(name, m[2]);
    if (closing) out += `</${name}>`;
    else if (VOID.has(name) || selfClosing) out += `<${name}${attrs}>`;
    else out += `<${name}${attrs}>`;
  }
  out += esc(String(html).slice(last));
  return out;
}