// web/src/lib/doctabs.js — the Tree directory-docs tab set (issue #170).
//
// Pure module (no Solid, no DOM): which tree entries become markdown tabs,
// their order (README first, rest alphabetical), the default selection, and
// the #anchor round-trip. Tree.jsx owns fetching + rendering; every rule
// here is unit-tested headless in web/test/unit/doctabs.test.js.

/** *.md / *.markdown, case-insensitive (the tab source set). */
export const MD_RE = /\.(md|markdown)$/i;

/** README with an md extension, case-insensitive: the first tab. */
export const README_RE = /^readme\.(md|markdown)$/i;

/** isMarkdownName(name) → true when the entry renders as a docs tab. */
export function isMarkdownName(name) {
  return MD_RE.test(String(name ?? ""));
}

/** isReadmeName(name) → true when the entry is the directory README. */
export function isReadmeName(name) {
  return README_RE.test(String(name ?? ""));
}

function compareNames(a, b) {
  const al = String(a).toLowerCase();
  const bl = String(b).toLowerCase();
  if (al < bl) return -1;
  if (al > bl) return 1;
  // Tie-break on the raw spelling so mixed-case duplicates order
  // deterministically across locales (localeCompare varies by ICU).
  if (a < b) return -1;
  if (a > b) return 1;
  return 0;
}

/**
 * orderDocFiles(names) → the tab order: README files first (alphabetical
 * among themselves, so readme.md + README.MARKDOWN is stable), then the
 * rest alphabetically (case-insensitive). Pure: never mutates its input.
 */
export function orderDocFiles(names) {
  const list = [...(names ?? [])];
  const readmes = list.filter(isReadmeName).sort(compareNames);
  const rest = list.filter((n) => !isReadmeName(n)).sort(compareNames);
  return [...readmes, ...rest];
}

/**
 * docCandidates(entries, readme) → markdown tab names for the CURRENT
 * directory listing. Source set: blob entries ending .md/.markdown. A
 * server-probed readme with a non-md spelling (the backend matches "readme"
 * with ANY extension) is appended so the old single-readme render never
 * regresses into nothing; it sorts first via orderDocFiles only when it is
 * itself an md spelling, otherwise it trails the md tabs.
 */
export function docCandidates(entries, readme) {
  const names = (entries ?? [])
    .filter((e) => e && e.type === "blob" && isMarkdownName(e.name))
    .map((e) => e.name);
  const out = orderDocFiles(names);
  const rname = readme?.name;
  if (rname && readme?.contents != null && !out.includes(rname)) out.push(rname);
  return out;
}

/** defaultDocFile(ordered) → the initially selected tab (README-first head), or null when empty. */
export function defaultDocFile(ordered) {
  return (ordered ?? []).length ? ordered[0] : null;
}

/**
 * docSlug(name) → the #anchor for a tab. encodeURIComponent keeps spaces,
 * quotes, unicode, and "#" itself out of the fragment so special-character
 * filenames can neither break the tab nor the anchor.
 */
export function docSlug(name) {
  return encodeURIComponent(String(name ?? ""));
}

/**
 * docBlobPath(dirPath, name) → the blob-endpoint path for a tab's file with
 * every segment percent-encoded. fetch() never sends a raw "#" (fragment
 * cut) and leaves spaces/parens to the URL parser; per-segment encoding
 * keeps those — plus "?", "%", and unicode — intact while "/" stays the
 * separator (the backend decodes one segment at a time). encodeURIComponent
 * is a no-op for ordinary names.
 */
export function docBlobPath(dirPath, name) {
  const segs = [];
  if (dirPath) segs.push(...String(dirPath).split("/"));
  segs.push(String(name ?? ""));
  return segs.map(encodeURIComponent).join("/");
}

/**
 * docFromHash(hash, ordered) → the tab named by a #anchor, or null when the
 * anchor is missing, undecodable, or names no tab in this directory.
 */
export function docFromHash(hash, ordered) {
  const raw = String(hash ?? "").replace(/^#/, "");
  if (!raw || !(ordered ?? []).length) return null;
  let name;
  try {
    name = decodeURIComponent(raw);
  } catch {
    return null; // malformed % sequence: fall back to the default tab
  }
  return ordered.includes(name) ? name : null;
}

/** A resolved commit revision: exactly 40 lowercase hex chars (backend shas). */
export const SHA_RE = /^[0-9a-f]{40}$/;

/**
 * docFetchArgs(rev, dirPath, name) → {rev, path} for the tab body's blob
 * fetch, or null when rev is not a resolved commit sha (issue #172).
 *
 * The ONLY acceptable revision is the tree payload's resolved commit sha —
 * the same resolution the tree listing itself uses. Ref names, short shas,
 * header-pill text, and cache-key fragments are UI display strings: the blob
 * route splits `{rev}` on "/" (a full ref name mangles into rev="refs"), so
 * interpolating one guarantees a 404 whose entry then sits on "loading…"
 * forever (sha-addressed entries never revalidate). A null return means
 * "do not fetch" — the caller renders loading without tray-spamming a
 * doomed request.
 */
export function docFetchArgs(rev, dirPath, name) {
  if (!SHA_RE.test(String(rev ?? ""))) return null;
  if (name == null || String(name) === "") return null;
  return { rev: String(rev), path: docBlobPath(dirPath ?? "", name) };
}
