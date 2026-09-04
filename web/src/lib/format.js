// web/src/lib/format.js — human-readable byte sizes for the Code tab (Tree entry
// sizes, Blob header / too-large / binary placeholders). Pure string ops —
// importable in Node (headless-testable). Exact byte counts stay in `title`
// tooltips at the call sites.

const UNITS = ["B", "k", "MB", "GB", "TB", "PB"];

/** fmtSize(n) → "47.2k" / "3MB" / "1GB"-style; null/undefined/NaN → "?". */
export function fmtSize(n) {
  if (n === null || n === undefined) return "?";
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return "?";
  if (v < 1024) return `${Math.floor(v)} B`;
  let scaled = v;
  let u = 0;
  while (scaled >= 1024 && u < UNITS.length - 1) {
    scaled /= 1024;
    u++;
  }
  const s = scaled >= 100 ? String(Math.round(scaled)) : String(Math.round(scaled * 10) / 10);
  return `${s}${UNITS[u]}`;
}
