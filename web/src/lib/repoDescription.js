// web/src/lib/repoDescription.js — issue #235 repo description helpers.
//
// Pure module (no Solid, no DOM): extract/set the top-level `description`
// key inside a repo-settings TOML document, plus the client-side shape check
// mirroring the server (`config.MaxRepoDescriptionRunes` = 512, single line).
// Headless-testable under `node --test`; Settings.jsx keeps the DOM.
//
/** Max description length, in characters (mirrors the server limit). */
export const MAX_DESCRIPTION_LENGTH = 512;

/**
 * Client-side shape check: null when ok, else a human-readable reason.
 * Non-strings and over-long/multi-line values are rejected before any PUT.
 */
export function validateDescription(d) {
  if (typeof d !== "string") return "description must be text";
  if (/[\r\n\0]/.test(d)) return "description must be a single line";
  if ([...d].length > MAX_DESCRIPTION_LENGTH) {
    return `description is ${[...d].length} characters; limit is ${MAX_DESCRIPTION_LENGTH}`;
  }
  return null;
}

/** Escape a value for a TOML basic string (quote + backslash + C0). */
export function escapeTomlBasic(d) {
  let out = "";
  for (const ch of d) {
    const cp = ch.codePointAt(0);
    if (ch === "\\") out += "\\\\";
    else if (ch === '"') out += '\\"';
    else if (cp < 0x20) out += `\\u${cp.toString(16).padStart(4, "0").toUpperCase()}`;
    else out += ch;
  }
  return out;
}

/** Unescape a TOML basic-string body (\\, \", \uXXXX, \UXXXXXXXX + \b\t\n\f\r). */
export function unescapeTomlBasic(body) {
  const simple = { b: "\b", t: "\t", n: "\n", f: "\f", r: "\r", '"': '"', "\\": "\\" };
  return body.replace(/\\(u[0-9A-Fa-f]{4}|U[0-9A-Fa-f]{8}|.)/g, (m, esc) => {
    if (esc.startsWith("u")) return String.fromCharCode(parseInt(esc.slice(1), 16));
    if (esc.startsWith("U")) return String.fromCodePoint(parseInt(esc.slice(1), 16));
    return simple[esc] ?? esc;
  });
}

// Top-level key scan: in TOML every bare key after a `[table]` header belongs
// to that table, so a top-level `description` can only live before the first
// section header. Scanning only that region keeps `[bundles]`-nested lookalikes
// (rejected server-side anyway) out of the read and the write.
function topLevelRegion(lines) {
  let end = lines.length;
  for (let i = 0; i < lines.length; i++) {
    if (/^\s*\[[^[\]]*\]\s*(#.*)?$/.test(lines[i])) {
      end = i;
      break;
    }
  }
  return end;
}

/**
 * Read the top-level `description` out of a settings TOML document.
 * "" when absent; basic (`"`) and literal (`'`) strings both read.
 */
export function extractDescription(tomlText) {
  if (typeof tomlText !== "string" || tomlText === "") return "";
  const lines = tomlText.split("\n");
  const end = topLevelRegion(lines);
  for (let i = 0; i < end; i++) {
    let m = lines[i].match(/^\s*description\s*=\s*"((?:[^"\\]|\\.)*)"\s*(#.*)?$/);
    if (m) return unescapeTomlBasic(m[1]);
    m = lines[i].match(/^\s*description\s*=\s*'([^']*)'\s*(#.*)?$/);
    if (m) return m[1];
  }
  return "";
}

/**
 * Return the document with the top-level `description` set to `desc`
 * (basic-string encoded). Replaces the existing top-level line in place;
 * otherwise inserts `description = "…"` as the first line. Never touches
 * section bodies.
 */
export function withDescription(tomlText, desc) {
  const line = `description = "${escapeTomlBasic(desc)}"`;
  if (typeof tomlText !== "string" || tomlText === "") return line + "\n";
  const lines = tomlText.split("\n");
  const end = topLevelRegion(lines);
  for (let i = 0; i < end; i++) {
    if (/^\s*description\s*=\s*("([^"\\]|\\.)*"|'[^']*')\s*(#.*)?$/.test(lines[i])) {
      lines[i] = line;
      return lines.join("\n");
    }
  }
  return `${line}\n${tomlText}`;
}
