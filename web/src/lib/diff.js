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

// --- review anchors (docs/features/04 §4/§8) ---------------------------------
//
// anchorContextSha(hunk, range) is the ONLY §4 drift-hash implementation on
// the client. Server twin: DriftHash in internal/review/model.go — both hash
// the same bytes (path + "\n" + up-to-3 context lines before + up-to-3 after,
// trailing-whitespace-trimmed, LF-joined), pinned by the same fixed vector in
// both test suites (the §8 dogfood rule: UI + server share semantics).
// Drift detection is derived at view time (hash mismatch ⇒ outdated,
// collapsed) — anchors are never relocated, never mutated.

/**
 * Collect up to 3 unchanged context lines (' ' rows) around an anchored
 * line range for the drift hash.
 * @param {{path: string, lines: Array<{t: string, text: string}>}} hunk parsed hunk with display path
 * @param {{start: number, count: number}} range index + length into hunk.lines
 * @returns {{path: string, before: string[], after: string[]}}
 */
export function anchorContext(hunk, range) {
  const lines = hunk?.lines ?? [];
  const start = Math.max(0, range?.start ?? 0);
  const count = Math.max(0, range?.count ?? 0);
  // Contiguous context immediately adjacent to the range (a -/+ run ends
  // the run — only true neighbors describe the anchor's position).
  const before = [];
  for (let i = start - 1; i >= 0 && lines[i]?.t === " "; i--) before.unshift(lines[i].text);
  const after = [];
  for (let i = start + count; i < lines.length && lines[i]?.t === " "; i++) after.push(lines[i].text);
  return { path: hunk?.path ?? "", before: before.slice(-3), after: after.slice(0, 3) };
}

/**
 * The §4 drift hash over an anchor's context. Sync (hand-rolled SHA-256 —
 * the SDK stays dependency-free and node --test has no SubtleCrypto).
 * @param {{path: string, lines: Array<{t: string, text: string}>}} hunk parsed hunk with display path
 * @param {{start: number, count: number}} range index + length into hunk.lines
 * @returns {string} 64-hex SHA-256
 */
export function anchorContextSha(hunk, range) {
  const { path, before, after } = anchorContext(hunk, range);
  let doc = `${path}\n`;
  for (const l of before) doc += `${String(l).replace(/[ \t\r]+$/, "")}\n`;
  for (const l of after) doc += `${String(l).replace(/[ \t\r]+$/, "")}\n`;
  return sha256hex(doc);
}

/** Minimal sync SHA-256 (UTF-8 in, 64-hex out). No dependencies. */
export function sha256hex(input) {
  const bytes = new TextEncoder().encode(input);
  const bitLenHi = Math.floor((bytes.length * 8) / 0x100000000);
  const bitLenLo = bytes.length * 8 >>> 0;
  const padded = [...bytes, 0x80];
  while (padded.length % 64 !== 56) padded.push(0);
  padded.push(
    (bitLenHi >>> 24) & 0xff, (bitLenHi >>> 16) & 0xff, (bitLenHi >>> 8) & 0xff, bitLenHi & 0xff,
    (bitLenLo >>> 24) & 0xff, (bitLenLo >>> 16) & 0xff, (bitLenLo >>> 8) & 0xff, bitLenLo & 0xff,
  );
  let h0 = 0x6a09e667, h1 = 0xbb67ae85, h2 = 0x3c6ef372, h3 = 0xa54ff53a;
  let h4 = 0x510e527f, h5 = 0x9b05688c, h6 = 0x1f83d9ab, h7 = 0x5be0cd19;
  const K = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
  ];
  const w = new Array(64);
  const rotr = (x, n) => (x >>> n) | (x << (32 - n));
  for (let off = 0; off < padded.length; off += 64) {
    for (let i = 0; i < 16; i++) {
      w[i] = ((padded[off + i * 4] << 24) | (padded[off + i * 4 + 1] << 16) | (padded[off + i * 4 + 2] << 8) | padded[off + i * 4 + 3]) >>> 0;
    }
    for (let i = 16; i < 64; i++) {
      const s0 = (rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3)) >>> 0;
      const s1 = (rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10)) >>> 0;
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }
    let [a, b, c, d, e, f, g, h] = [h0, h1, h2, h3, h4, h5, h6, h7];
    for (let i = 0; i < 64; i++) {
      const S1 = (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) >>> 0;
      const ch = ((e & f) ^ (~e & g)) >>> 0;
      const t1 = (h + S1 + ch + K[i] + w[i]) >>> 0;
      const S0 = (rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) >>> 0;
      const maj = ((a & b) ^ (a & c) ^ (b & c)) >>> 0;
      const t2 = (S0 + maj) >>> 0;
      h = g; g = f; f = e; e = (d + t1) >>> 0; d = c; c = b; b = a; a = (t1 + t2) >>> 0;
    }
    h0 = (h0 + a) >>> 0; h1 = (h1 + b) >>> 0; h2 = (h2 + c) >>> 0; h3 = (h3 + d) >>> 0;
    h4 = (h4 + e) >>> 0; h5 = (h5 + f) >>> 0; h6 = (h6 + g) >>> 0; h7 = (h7 + h) >>> 0;
  }
  return [h0, h1, h2, h3, h4, h5, h6, h7].map((x) => x.toString(16).padStart(8, "0")).join("");
}
