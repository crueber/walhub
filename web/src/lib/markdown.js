// web/src/lib/markdown.js — hand-rolled markdown-lite (§2.2): headings, paragraphs,
// fenced code, inline code, bold/italic, links + autolinks, GFM tables, blockquotes,
// lists (nested one level), hr, images. Line-based emitter feeding the sanitizer;
// preview fidelity is preview-level (the code view shows exact text).
// Importable in Node, no DOM.

function esc(s) {
  return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

/** Inline pass: code → strong → em → images → links → autolinks, over escaped text. */
function inline(text) {
  let out = esc(text);
  const stash = [];
  const keep = (html) => `\u0000${stash.push(html) - 1}\u0000`; // protect emitted tags from later passes
  out = out.replace(/`([^`]+)`/g, (_, c) => keep(`<code>${c}</code>`));
  out = out.replace(/\*\*([^*]+)\*\*/g, (_, c) => keep(`<strong>${c}</strong>`));
  out = out.replace(/(^|[\s(])\*([^*\s][^*]*)\*/g, (_, pre, c) => keep(`${pre}<em>${c}</em>`));
  out = out.replace(/(^|[\s(])_([^_\s][^_]*)_/g, (_, pre, c) => keep(`${pre}<em>${c}</em>`));
  out = out.replace(/!\[([^\]]*)\]\(([^)\s]+)(?:\s+&quot;([^&]*)&quot;)?\)/g, (_, alt, src, title) =>
    keep(`<img src="${src}" alt="${alt}"${title ? ` title="${title}"` : ""}>`));
  out = out.replace(/\[([^\]]+)\]\(([^)\s]+)(?:\s+&quot;([^&]*)&quot;)?\)/g, (_, label, href, title) =>
    keep(`<a href="${href}"${title ? ` title="${title}"` : ""}>${label}</a>`));
  out = out.replace(/\bhttps?:\/\/[^\s<>&\u0000]+[^\s<>&\u0000.,;:!?)\]]/g, (url) => keep(`<a href="${url}">${url}</a>`));
  out = out.replace(/\b[\w.+-]+@[\w-]+\.[\w.-]+\b/g, (mail) => keep(`<a href="mailto:${mail}">${mail}</a>`));
  return out.replace(/\u0000(\d+)\u0000/g, (_, i) => stash[Number(i)]);
}

const HR_RE = /^\s{0,3}(?:-{3,}|\*{3,}|_{3,})\s*$/;
const H_RE = /^\s{0,3}(#{1,6})\s+(.*)$/;
const BQ_RE = /^\s{0,3}>\s?(.*)$/;
const UL_RE = /^(\s*)[-*+]\s+(.*)$/;
const OL_RE = /^(\s*)\d+[.)]\s+(.*)$/;
const TABLE_SEP_RE = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?\s*$/;

function splitRow(line) {
  return line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => c.trim());
}

/** renderMarkdown(src) → HTML string (sanitizer input, never trusted raw). */
export function renderMarkdown(src) {
  const lines = String(src ?? "").replace(/\r\n?/g, "\n").split("\n");
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    if (line.trim() === "") { i++; continue; } // blank line separator

    // fenced code block
    const fence = /^\s{0,3}(```+|~~~+)\s*(\S*)\s*$/.exec(line);
    if (fence) {
      const marker = fence[1][0];
      const lang = fence[2];
      const body = [];
      i++;
      while (i < lines.length && !new RegExp(`^\\s{0,3}${marker === "`" ? "`{3,}" : "~{3,}"}\\s*$`).test(lines[i])) {
        body.push(lines[i]);
        i++;
      }
      i++; // closing fence (or EOF)
      out.push(`<pre><code${lang ? ` data-lang="${esc(lang)}"` : ""}>${esc(body.join("\n"))}\n</code></pre>`);
      continue;
    }

    // ATX heading
    const h = H_RE.exec(line);
    if (h) { out.push(`<h${h[1].length}>${inline(h[2].trim())}</h${h[1].length}>`); i++; continue; }

    // hr
    if (HR_RE.test(line)) { out.push("<hr>"); i++; continue; }

    // blockquote (consecutive > lines)
    if (BQ_RE.test(line)) {
      const body = [];
      while (i < lines.length && BQ_RE.test(lines[i])) { body.push(BQ_RE.exec(lines[i])[1]); i++; }
      out.push(`<blockquote>${renderMarkdown(body.join("\n"))}</blockquote>`);
      continue;
    }

    // GFM table: header row + separator row
    if (line.includes("|") && i + 1 < lines.length && TABLE_SEP_RE.test(lines[i + 1])) {
      const head = splitRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim() !== "") { rows.push(splitRow(lines[i])); i++; }
      out.push(
        "<table><thead><tr>" + head.map((c) => `<th>${inline(c)}</th>`).join("") + "</tr></thead><tbody>" +
        rows.map((r) => "<tr>" + head.map((_, ci) => `<td>${inline(r[ci] ?? "")}</td>`).join("") + "</tr>").join("") +
        "</tbody></table>"
      );
      continue;
    }

    // lists (nested one level): 4-space (or 2+) indent under an open item = child list
    const ul = UL_RE.exec(line);
    const ol = OL_RE.exec(line);
    if (ul || ol) {
      const ordered = Boolean(ol);
      const re = ordered ? OL_RE : UL_RE;
      out.push(renderList(lines, re, i, ordered, (next) => { i = next; }));
      continue;
    }

    // paragraph: gather until blank or a block opener
    const para = [line];
    i++;
    while (i < lines.length && lines[i].trim() !== "" && !H_RE.test(lines[i]) && !HR_RE.test(lines[i]) &&
           !UL_RE.test(lines[i]) && !OL_RE.test(lines[i]) && !BQ_RE.test(lines[i]) && !/^\s{0,3}(```|~~~)/.test(lines[i])) {
      para.push(lines[i]);
      i++;
    }
    out.push(`<p>${para.map((p) => inline(p)).join("<br>")}</p>`);
  }
  return out.join("\n");
}

function renderList(lines, re, start, ordered, seek) {
  const items = [];
  let i = start;
  let childIndent = null;
  while (i < lines.length) {
    const m = re.exec(lines[i]);
    if (m) {
      // nested one level: an indented item under an open item becomes its child
      if (m[1].length > 0 && items.length && (childIndent === null || m[1].length >= childIndent)) {
        childIndent ??= m[1].length;
        items[items.length - 1].children.push(m[2]);
        i++;
        continue;
      }
      if (childIndent !== null && m[1].length < childIndent) break;
      items.push({ text: m[2], children: [] });
      i++;
      continue;
    }
    const blank = lines[i].trim() === "";
    const child = /^(\s+)(.*)$/.exec(lines[i]);
    const childIsItem = child && (UL_RE.test(lines[i]) || OL_RE.test(lines[i]));
    if (childIsItem && items.length && (childIndent === null || child[1].length >= childIndent)) {
      childIndent ??= child[1].length;
      // nested one level only: fold deeper indents into the child list
      items[items.length - 1].children.push(child[2].replace(UL_RE, "$2").replace(OL_RE, "$2"));
      i++;
      continue;
    }
    if (blank) {
      // a blank inside a list ends it unless the next line continues an item
      if (i + 1 < lines.length && (UL_RE.test(lines[i + 1]) || OL_RE.test(lines[i + 1]))) { i++; continue; }
      break;
    }
    if (items.length && /^\s{2,}\S/.test(lines[i])) { // continuation line of the last item
      items[items.length - 1].text += " " + lines[i].trim();
      i++;
      continue;
    }
    break;
  }
  seek(i);
  const tag = ordered ? "ol" : "ul";
  const body = items
    .map((it) => `<li>${inline(it.text)}${it.children.length ? `<${tag}>${it.children.map((c) => `<li>${inline(c)}</li>`).join("")}</${tag}>` : ""}</li>`)
    .join("");
  return `<${tag}>${body}</${tag}>`;
}
