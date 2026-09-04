// web/src/lib/format.js — human-readable sizes + modes for the Code tab (Tree
// entry sizes/modes, Blob header / too-large / binary placeholders). Pure
// string ops — importable in Node (headless-testable). Exact byte counts stay
// in `title` tooltips at the call sites, as do raw octal modes.

const UNITS = ["b", "k", "MB", "GB", "TB", "PB"];

/** fmtSize(n) → "92b" / "47.2k" / "3MB" / "1GB"-style; null/undefined/NaN → "?". */
export function fmtSize(n) {
  if (n === null || n === undefined) return "?";
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return "?";
  if (v < 1024) return `${Math.floor(v)}b`;
  let scaled = v;
  let u = 0;
  while (scaled >= 1024 && u < UNITS.length - 1) {
    scaled /= 1024;
    u++;
  }
  const s = scaled >= 100 ? String(Math.round(scaled)) : String(Math.round(scaled * 10) / 10);
  return `${s}${UNITS[u]}`;
}

// --- git modes as rwx triplets ------------------------------------------------
// Tree entry modes arrive as octal strings ("100644"). Regular files and
// directories map onto their permission triplets; the two special object
// types carry no permission bits, so they get fixed glyphs:
//
//   100644 → rw-r--r--    100755 → rwxr-xr-x    040000/40000 → rwxr-xr-x
//   120000 (symlink) → rwxrwxrwx (a link resolves to whatever it points at)
//   160000 (gitlink) → m--------- ("m" marks a submodule commit pointer; the
//                        dashes mark "no permission bits" — decided, issue #29)
const MODE_GLYPHS = {
  "100644": "rw-r--r--",
  "100755": "rwxr-xr-x",
  "120000": "rwxrwxrwx",
  "160000": "m---------",
  "40000": "rwxr-xr-x",
  "040000": "rwxr-xr-x",
};

/**
 * fmtMode(mode) → "rw-r--r--"-style. Known git modes map via the table above;
 * any other octal falls back to its low 9 permission bits; missing/blank or
 * non-octal input renders as "" (call sites leave the cell empty).
 */
export function fmtMode(mode) {
  if (mode === undefined || mode === null) return "";
  const s = String(mode).trim();
  if (s === "") return "";
  if (MODE_GLYPHS[s] !== undefined) return MODE_GLYPHS[s];
  const digits = s.replace(/[^0-7]/g, "").slice(-3);
  if (digits === "") return "";
  let out = "";
  for (const c of digits) {
    const d = Number(c);
    out += (d & 4 ? "r" : "-") + (d & 2 ? "w" : "-") + (d & 1 ? "x" : "-");
  }
  return out;
}
