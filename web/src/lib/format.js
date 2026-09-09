// web/src/lib/format.js — human-readable sizes + modes for the Code tab (Tree
// entry sizes/modes, Blob header / too-large / binary placeholders) plus the
// app-wide date display (issue #133). Pure string ops — importable in Node
// (headless-testable). Exact byte counts stay in `title` tooltips at the call
// sites, as do raw octal modes; exact wall-clock times live in the
// `fmtDateTitle` tooltip rendered by `<DateTime>`.

const UNITS = ["b", "k", "MB", "GB", "TB", "PB"];

// scaleSteps(v) → { text, step }: the shared 1024-step ladder behind fmtSize
// and fmtSizeParts. step 0 = bytes (integer text); higher steps divide by
// 1024 with ≥100 → integer else one trimmed decimal. Extracted (issue #211)
// so the two size spellings cannot drift apart; fmtSize output is unchanged.
function scaleSteps(v) {
  if (v < 1024) return { text: String(Math.floor(v)), step: 0 };
  let scaled = v;
  let step = 0;
  while (scaled >= 1024 && step < UNITS.length - 1) {
    scaled /= 1024;
    step++;
  }
  const text = scaled >= 100 ? String(Math.round(scaled)) : String(Math.round(scaled * 10) / 10);
  return { text, step };
}

/** fmtSize(n) → "92b" / "47.2k" / "3MB" / "1GB"-style; null/undefined/NaN → "?". */
export function fmtSize(n) {
  if (n === null || n === undefined) return "?";
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return "?";
  const { text, step } = scaleSteps(v);
  return `${text}${UNITS[step]}`;
}

// --- split size columns (issue #211) ------------------------------------------
// The Code tab shows the number and the unit in their own columns ("414"
// "B", "17" "KB", "18" "MB"). Units are the uppercase issue spellings over
// 1024-steps (B = bytes, KB = KiB, … — NOT SI/decimal). fmtSize keeps its
// #27/#29 lowercase single-string form ("92b"/"47.2k") for inline call sites
// (Blob header, doctabs notes), so the two spellings coexist deliberately:
// one string → fmtSize, two columns → fmtSizeParts. Same ladder and same
// rounding in both (via scaleSteps). Invalid input → { num: "?", unit: "" }
// so the number cell still shows the "?" placeholder and the unit stays
// empty.
const SIZE_UNITS = ["B", "KB", "MB", "GB", "TB", "PB"];

/**
 * fmtSizeParts(n) → { num, unit } e.g. { num: "47.2", unit: "KB" }.
 * null/undefined/NaN/negative → { num: "?", unit: "" }.
 */
export function fmtSizeParts(n) {
  if (n === null || n === undefined) return { num: "?", unit: "" };
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return { num: "?", unit: "" };
  const { text, step } = scaleSteps(v);
  return { num: text, unit: SIZE_UNITS[step] };
}

// --- git modes as ls-style rows (issue #211) ------------------------------------
// Tree entry modes arrive as octal strings ("100644"). The column renders a
// 10-char ls -la-style cell: a leading type char plus the 9-char permission
// triplet (issue examples: `.rw-rw-r--`, `drwxrwxr-x`):
//
//   100644 → .rw-r--r--    100755 → .rwxr-xr-x    040000/40000 → drwxr-xr-x
//   120000 (symlink) → lrwxrwxrwx (a link resolves to whatever it points at)
//   160000 (gitlink) → m--------- (unchanged from #29: the `m` that marked a
//                        submodule commit pointer IS the leading type char,
//                        and the nine dashes mark "no permission bits")
//
// Leading-char decisions (documented per AGENTS.md law 12):
// - Regular files take `.`, not ls's `-`: the issue's example is `.rw-rw-r--`
//   (eza-style), and `-` already means "missing size" elsewhere in this table.
// - Symlinks take `l` (ls convention): their type from ls-tree is "blob", so
//   the mode (120000) — not the type — is what detects them.
// - Submodules take `m` (kept from #29), not `d`: a gitlink is a commit
//   pointer, not a navigable directory listing.
// - `type` (the ls-tree "blob"|"tree"|"commit" string) only disambiguates
//   non-canonical mode strings on the fallback path; canonical modes decide
//   alone, so fmtMode("040000") is "drwxr-xr-x" with no second argument.
const MODE_GLYPHS = {
  "100644": "rw-r--r--",
  "100755": "rwxr-xr-x",
  "120000": "rwxrwxrwx",
  "160000": "---------", // the lead `m` comes from modeLead; the body marks "no bits"
  "40000": "rwxr-xr-x",
  "040000": "rwxr-xr-x",
};

/**
 * fmtMode(mode, type?) → ".rw-r--r--"-style (leading type char + 9 permission
 * chars). Known git modes map via the table above; any other octal falls
 * back to its low 9 permission bits with the lead from `type`
 * ("tree" → d, "commit" → m, else "."); missing/blank or non-octal input
 * renders as "" (call sites leave the cell empty).
 */
export function fmtMode(mode, type) {
  if (mode === undefined || mode === null) return "";
  const s = String(mode).trim();
  if (s === "") return "";
  const body = fmtModeBody(s);
  if (body === "") return "";
  return modeLead(s, type) + body;
}

// modeLead(s, type) → the ls-style leading char: d = tree, l = symlink,
// m = submodule/gitlink, . = regular file. Canonical modes decide alone;
// `type` only covers non-canonical mode strings (e.g. a bare "755" from a
// tree object still leads `d`).
function modeLead(s, type) {
  if (s === "040000" || s === "40000" || type === "tree") return "d";
  if (s === "160000" || type === "commit") return "m";
  if (s === "120000") return "l";
  return ".";
}

// fmtModeBody(s) → the 9 permission chars (glyph table, else low-3-bits
// fallback); "" when s carries no octal digits.
function fmtModeBody(s) {
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

// --- app-wide date display (issue #133) --------------------------------------
// Every date in the UI renders through `fmtDate` (text) inside `<DateTime>`
// (a `<time>` element whose `title` is `fmtDateTitle`). Three tiers:
//
//   age < 1 day   → relative only: "just now" (< 60 s), "N minute(s) ago"
//                   (< 60 min), "N hour(s) ago" (< 24 h)
//   1–30 days     → "N day(s) ago - {ordinal} of {Month}"
//                   (e.g. "3 days ago - 2nd of September")
//   31+ days      → "{Month} {ordinal}, {Year}" (e.g. "September 28th, 2024")
//
// Decisions (documented per AGENTS.md law 12):
// - "month" is a fixed 31-day boundary, not a calendar month: whole-day age
//   ≤ 30 renders the middle tier, ≥ 31 the absolute form. Calendar months
//   vary (28–31 days), which would make the tier flip on unknowable days.
// - Future timestamps clamp to "just now" (clock skew / optimistic rows must
//   never render "-3 minutes ago").
// - Month names are fixed English (not Intl locale names) so the tier text is
//   deterministic in every browser locale; only the hover title localizes.
// - The hover title is the user's LOCAL wall time "YYYY-MM-DD HH:MM <zone>"
//   with the real zone abbreviation from Intl (`timeZoneName: "short"` —
//   e.g. "CDT", "CET", "GMT+2"), never UTC-suffixed. When Intl yields no zone
//   name, a GMT±H[:MM] offset computed from getTimezoneOffset is used.
// - Fallbacks are unchanged from the old per-page formatters: falsy input →
//   "", unparseable input → String(input) (both text and title).

const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

/** ordinal(n) → "1st"/"2nd"/"3rd"/"4th"/…; 11/12/13 always take "th". */
export function ordinal(n) {
  const v = Math.trunc(Number(n));
  if (!Number.isFinite(v)) return `${n}th`;
  const lastTwo = Math.abs(v) % 100;
  if (lastTwo >= 11 && lastTwo <= 13) return `${v}th`;
  switch (Math.abs(v) % 10) {
    case 1: return `${v}st`;
    case 2: return `${v}nd`;
    case 3: return `${v}rd`;
    default: return `${v}th`;
  }
}

function toDate(input) {
  if (input === undefined || input === null || input === "") return null;
  const d = input instanceof Date ? input : new Date(input);
  return Number.isNaN(d.getTime()) ? null : d;
}

function plural(n, word) {
  return `${n} ${word}${n === 1 ? "" : "s"} ago`;
}

/**
 * fmtDate(iso, now?) → tier text per the rules above. Falsy → "", invalid →
 * String(iso). `now` (ms epoch) exists for tests; call sites omit it.
 */
export function fmtDate(iso, now = Date.now()) {
  if (iso === undefined || iso === null || iso === "") return "";
  const d = toDate(iso);
  if (!d) return String(iso);
  const age = Math.max(0, now - d.getTime());
  if (age < MIN) return "just now";
  if (age < HOUR) return plural(Math.floor(age / MIN), "minute");
  if (age < DAY) return plural(Math.floor(age / HOUR), "hour");
  const days = Math.floor(age / DAY);
  const monthDay = `${ordinal(d.getDate())} of ${MONTHS[d.getMonth()]}`;
  if (days <= 30) return `${plural(days, "day")} - ${monthDay}`;
  return `${MONTHS[d.getMonth()]} ${ordinal(d.getDate())}, ${d.getFullYear()}`;
}

function pad2(n) {
  return String(n).padStart(2, "0");
}

function zoneName(d) {
  try {
    const parts = new Intl.DateTimeFormat("en-US", { timeZoneName: "short" }).formatToParts(d);
    const tz = parts.find((p) => p.type === "timeZoneName")?.value;
    if (tz) return tz;
  } catch {
    // fall through to the manual offset below
  }
  const off = -d.getTimezoneOffset();
  const sign = off < 0 ? "-" : "+";
  const abs = Math.abs(off);
  const hh = Math.floor(abs / 60);
  const mm = abs % 60;
  return `GMT${sign}${hh}${mm ? `:${pad2(mm)}` : ""}`;
}

/**
 * fmtDateTitle(iso) → "YYYY-MM-DD HH:MM <zone>" in the user's local timezone
 * for the `<time title>`. Falsy → "", invalid → String(iso).
 */
export function fmtDateTitle(iso) {
  if (iso === undefined || iso === null || iso === "") return "";
  const d = toDate(iso);
  if (!d) return String(iso);
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ` +
    `${pad2(d.getHours())}:${pad2(d.getMinutes())} ${zoneName(d)}`;
}

/**
 * dateTimeAttr(value) → machine-readable `<time dateTime>`: valid inputs
 * normalize to UTC ISO ("2025-03-04T08:23:00.000Z") so Date objects and epoch
 * numbers are as correct as ISO strings; unparseable input falls back to
 * String(value) (never throws — `new Date(bad).toISOString()` would).
 */
export function dateTimeAttr(value) {
  try {
    const d = value instanceof Date ? value : new Date(value);
    if (!Number.isNaN(d.getTime())) return d.toISOString();
  } catch {
    // fall through to the String fallback below
  }
  return String(value);
}
