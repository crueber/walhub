// web/src/lib/diff.js — §2.8: hand-rolled minimal unified-diff parser. The server
// sends a single well-formed `git diff` patch (spec §9.5: unified vs first parent,
// --no-color, rename detection), so a tiny parser with an explicit grammar is
// smaller than any library.
//
// patch  := header+ body* EOF
// header := "diff --git a/<path> b/<path>" LF (meta)* ("--- …") LF ("+++ …") LF
// body   := (" " line | "-" line | "+" line | LF)*   under "@@ -o[,n] +p[,q] @@ ctx?"

const FILE_RE = /^diff --git a\/(.+) b\/(.+)$/;
const HUNK_RE = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: (.+))?$/;
const META_PREFIXES = [
  "old mode", "new mode", "deleted file mode", "new file mode", "copy from", "copy to",
  "rename from", "rename to", "similarity index", "dissimilarity index", "index ",
];

function stripRef(p) {
  // "a/<path>" / "b/<path>" → "<path>"; quoted paths keep their quotes (rare, preview-level)
  return p.replace(/^[ab]\//, "");
}

/** parsePatchFiles(patch, sha) → {files:[{path, oldPath?, added?, deleted?, isBinary?, hunks:[…]}]} */
export function parsePatchFiles(patch /* , sha */) {
  const files = [];
  if (!patch) return { files };
  const lines = patch.split("\n");
  if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop(); // artifact of the trailing newline
  let f = null;
  const push = () => {
    if (!f) return;
    let additions = 0;
    let deletions = 0;
    for (const h of f.hunks) {
      for (const l of h.lines) {
        if (l.t === "+") additions++;
        else if (l.t === "-") deletions++;
      }
    }
    f.additions = additions;
    f.deletions = deletions;
    files.push(f);
  };

  for (let i = 0; i < lines.length; ) {
    const line = lines[i];

    if (line.startsWith("diff --git ")) {
      push();
      const m = FILE_RE.exec(line);
      f = { path: m ? m[2] : line.slice(11), oldPath: null, added: false, deleted: false, isBinary: false, hunks: [] };
      i++;
      while (i < lines.length) {
        const l = lines[i];
        if (l.startsWith("rename from ")) { f.oldPath = stripRef(l.slice(12)); i++; continue; }
        if (l.startsWith("rename to ")) { f.path = stripRef(l.slice(10)); i++; continue; }
        if (l.startsWith("copy from ")) { f.oldPath = stripRef(l.slice(10)); i++; continue; }
        if (l.startsWith("copy to ")) { f.path = stripRef(l.slice(8)); i++; continue; }
        if (l.startsWith("deleted file mode")) { f.deleted = true; i++; continue; }
        if (l.startsWith("new file mode")) { f.added = true; i++; continue; }
        if (l.startsWith("--- ")) {
          const old = l.slice(4).replace(/\t$/, "").trim();
          f.oldPath = old === "/dev/null" ? null : stripRef(old);
          i++;
          if (i < lines.length && lines[i].startsWith("+++ ")) {
            const nw = lines[i].slice(4).replace(/\t$/, "").trim();
            if (nw === "/dev/null") f.deleted = true;
            else f.path = stripRef(nw);
            i++;
          }
          continue;
        }
        if (META_PREFIXES.some((p) => l.startsWith(p))) { i++; continue; }
        break;
      }
      continue;
    }

    if (f) {
      if (line.startsWith("Binary files ") && line.endsWith(" differ")) { f.isBinary = true; i++; continue; }
      if (line.startsWith("GIT binary patch")) { f.isBinary = true; i++; continue; }
      const h = HUNK_RE.exec(line);
      if (h) {
        f.hunks.push({
          oldStart: Number(h[1]),
          oldLines: h[2] === undefined ? 1 : Number(h[2]),
          newStart: Number(h[3]),
          newLines: h[4] === undefined ? 1 : Number(h[4]),
          context: h[5] ?? "",
          lines: [],
        });
        i++;
        continue;
      }
      const cur = f.hunks[f.hunks.length - 1];
      if (cur) {
        const t = line[0];
        if (t === "+" || t === "-" || t === " ") {
          cur.lines.push({ t, text: line.slice(1) });
          i++;
          continue;
        }
        if (line === "") { cur.lines.push({ t: " ", text: "" }); i++; continue; } // bare LF = context
        if (line.startsWith("\\")) { i++; continue; } // "\ No newline at end of file"
      }
    }
    i++; // unknown line: skip (grammar says terminal, be lenient with trailing junk)
  }
  push();
  return { files };
}

/**
 * splitRows(lines, window=20) — build split-view rows by pairing "-" / "+"
 * runs via an LCS window (hand-rolled). Returns [{left:{t,text}|null, right:…|null}].
 */
export function splitRows(lines, windowSize = 20) {
  const rows = [];
  let i = 0;
  while (i < lines.length) {
    const t = lines[i].t;
    if (t === " ") {
      rows.push({ left: { t: " ", text: lines[i].text }, right: { t: " ", text: lines[i].text } });
      i++;
      continue;
    }
    const dels = [];
    const adds = [];
    while (i < lines.length && lines[i].t === "-") dels.push(lines[i++].text);
    while (i < lines.length && lines[i].t === "+") adds.push(lines[i++].text);
    while (dels.length || adds.length) {
      const d = dels.splice(0, windowSize);
      const a = adds.splice(0, windowSize);
      rows.push(...lcsPair(d, a));
    }
  }
  return rows;
}

function lcsPair(dels, adds) {
  const n = dels.length;
  const m = adds.length;
  if (!n) return adds.map((text) => ({ left: null, right: { t: "+", text } }));
  if (!m) return dels.map((text) => ({ left: { t: "-", text }, right: null }));
  // LCS table (windowed to ≤ 20×20 by the caller)
  const dp = Array.from({ length: n + 1 }, () => new Uint16Array(m + 1));
  for (let a = n - 1; a >= 0; a--) {
    for (let b = m - 1; b >= 0; b--) {
      dp[a][b] = dels[a] === adds[b] ? dp[a + 1][b + 1] + 1 : Math.max(dp[a + 1][b], dp[a][b + 1]);
    }
  }
  const rows = [];
  let a = 0;
  let b = 0;
  while (a < n && b < m) {
    if (dels[a] === adds[b]) {
      rows.push({ left: { t: "-", text: dels[a] }, right: { t: "+", text: adds[b] } });
      a++; b++;
    } else if (dp[a + 1][b] >= dp[a][b + 1]) {
      rows.push({ left: { t: "-", text: dels[a] }, right: null });
      a++;
    } else {
      rows.push({ left: null, right: { t: "+", text: adds[b] } });
      b++;
    }
  }
  while (a < n) rows.push({ left: { t: "-", text: dels[a++] }, right: null });
  while (b < m) rows.push({ left: null, right: { t: "+", text: adds[b++] } });
  return rows;
}

// --- commit-body formatting (§2.8: linkified bodies, grouped trailers) -------

const SHA_RE = /^[0-9a-f]{40}$|^[0-9a-f]{64}$/;
const MAIL_RE = /^(.*?)\s*<([^>]+@[^>]+)>\s*$/;
const URL_RE = /\bhttps?:\/\/[^\s<>")\]]+/g;
const TRAILING_PUNCT = /[.,;:!?]+$/;

function esc(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

function htmlOf(text, repoBase) {
  // escape first, then wrap the two sanctioned link shapes back in
  let out = "";
  let last = 0;
  for (const m of text.matchAll(URL_RE)) {
    out += esc(text.slice(last, m.index));
    let url = m[0];
    const punct = TRAILING_PUNCT.exec(url.slice(-1)) ? url[url.length - 1] : "";
    if (punct) url = url.slice(0, -1);
    out += `<a href="${esc(url)}" rel="noopener">${esc(url)}</a>${esc(punct)}`;
    last = m.index + m[0].length;
  }
  out += esc(text.slice(last));
  if (repoBase) {
    out = out.replace(/\b([0-9a-f]{40}|[0-9a-f]{64})\b/g, (sha) =>
      `<a class="sha" href="${esc(repoBase)}/commit/${sha}" title="Commit">${sha.slice(0, 12)}</a>`
    );
  }
  return out;
}

/** Linkify a commit body: bare URLs → anchors, 40/64-hex shas → commit/:sha links. */
export function linkifyBody(text, repoBase) {
  return htmlOf(text ?? "", repoBase);
}

const PEOPLE_KEYS = new Set(["co-authored-by", "assisted-by", "signed-off-by", "reviewed-by", "acked-by", "tested-by"]);
const GROUPS = ["Merge queue", "People", "Other"];

/** Group trailers per §2.8: People / merge-queue keys / Other. */
export function groupTrailers(trailers) {
  const grouped = { "Merge queue": [], People: [], Other: [] };
  for (const t of trailers ?? []) {
    const k = String(t.key ?? "").toLowerCase();
    if (PEOPLE_KEYS.has(k)) grouped.People.push(t);
    else if (k.startsWith("merge-queue-") || k.includes("ci-sha") || k.includes("ci-boundary")) grouped["Merge queue"].push(t);
    else grouped.Other.push(t);
  }
  return GROUPS.filter((g) => grouped[g].length).map((g) => ({ group: g, trailers: grouped[g] }));
}

/** Parse a trailer value into {name?, email?, sha?} for rendering. */
export function trailerValue(value) {
  const v = String(value ?? "").trim();
  if (SHA_RE.test(v)) return { sha: v };
  const m = MAIL_RE.exec(v);
  if (m) return { name: m[1], email: m[2] };
  return { text: v };
}
