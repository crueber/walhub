// web/src/lib/repoReview.js — Forgejo #586 per-repo code-review policy.
//
// Pure module (no Solid, no DOM): read/write the `[review]` section's
// `allow_self_approval` knob of a repo-settings TOML document for the
// General settings tab. Headless-testable under `node --test`;
// Settings.jsx keeps the DOM.
//
// Default-ON is the whole contract: an unset knob (null — absent section
// or absent key) renders checked, because the server default is allowed
// (fresh repos behave like GitHub). Only an explicit `false` denies.

/** Human label for the General settings tab. */
export const SELF_APPROVAL_LABEL = "Allow self-approval";

/** One-line consequence hint for the General settings tab. */
export const SELF_APPROVAL_HINT =
  "Authors may approve or request changes on their own pull requests (a self-approval never counts toward required reviews)";

/**
 * Read the `[review]` section's `allow_self_approval` out of a settings
 * TOML document for the settings editor prefill. Returns
 * `true` / `false` / `null` — null = unset (renders checked: the server
 * default is allowed). Only `true` / `false` literals read; any other
 * spelling stays null (the server rejects it on save anyway, and the
 * editor never writes it).
 */
export function extractSelfApproval(tomlText) {
  if (typeof tomlText !== "string" || tomlText === "") return null;
  const lines = tomlText.split("\n");
  let inReview = false;
  for (const line of lines) {
    const header = line.match(/^\s*\[([^\]]*)\]\s*(#.*)?$/);
    if (header) {
      inReview = header[1].trim() === "review";
      continue;
    }
    if (!inReview) continue;
    const m = line.match(/^\s*allow_self_approval\s*=\s*(true|false)\s*(#.*)?$/);
    if (m) return m[1] === "true";
  }
  return null;
}

/**
 * Return the document with the `[review]` section's `allow_self_approval`
 * set to `allowed`. An existing `allow_self_approval` line is replaced
 * in place; otherwise the line is appended to the existing `[review]`
 * block (sibling lines pass through untouched); with no `[review]`
 * block the canonical block is appended. Every other section —
 * including a top-level `description` line and the `[features]` block —
 * passes through untouched.
 */
export function withSelfApproval(tomlText, allowed) {
  const line = `allow_self_approval = ${allowed === false ? "false" : "true"}`;
  const text = typeof tomlText === "string" ? tomlText : "";
  if (text === "") return `[review]\n${line}\n`;
  const lines = text.split("\n");
  let start = -1;
  let end = lines.length;
  for (let i = 0; i < lines.length; i++) {
    const header = lines[i].match(/^\s*\[([^\]]*)\]\s*(#.*)?$/);
    if (!header) continue;
    if (header[1].trim() === "review" && start === -1) {
      start = i;
      continue;
    }
    if (start !== -1) {
      end = i;
      break;
    }
  }
  if (start === -1) {
    const sep = text.endsWith("\n") ? "" : "\n";
    return `${text}${sep}[review]\n${line}\n`;
  }
  for (let i = start + 1; i < end; i++) {
    if (/^\s*allow_self_approval\s*=/.test(lines[i])) {
      return [...lines.slice(0, i), line, ...lines.slice(i + 1)].join("\n");
    }
  }
  return [...lines.slice(0, end), line, ...lines.slice(end)].join("\n");
}
