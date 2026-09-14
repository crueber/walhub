// web/src/lib/repoFeatures.js — Forgejo #522 per-repo feature flags.
//
// Pure module (no Solid, no DOM): resolve the summary `features` object
// (six enabled booleans, always present on #522 servers) with fail-open
// semantics, and read/write the `[features]` section of a repo-settings
// TOML document for the General settings tab. Headless-testable under
// `node --test`; Repo.jsx/Settings.jsx keep the DOM.
//
// Fail-open is the whole contract: a loading summary (undefined), a
// deleted repo (null), or a pre-#522 server (no field) must never hide a
// tab or disable a pill — only an explicit `false` disables. (The server
// mirrors this: unpopulated views project all-on.)

/** The six flags, in the wire/TOML order (matches config.ResolvedFeatures). */
export const FEATURE_KEYS = ["issues", "pulls", "releases", "forks", "watch", "star"];

/** Human labels for the General settings tab, in FEATURE_KEYS order. */
export const FEATURE_LABELS = {
  issues: "Issues",
  pulls: "Pull requests",
  releases: "Releases",
  forks: "Forking",
  watch: "Watching",
  star: "Starring",
};

/** One-line consequence hints for the General settings tab. */
export const FEATURE_HINTS = {
  issues: "Show the Issues tab and allow new issues",
  pulls: "Show the Pulls tab and allow new pull requests",
  releases: "Show the Releases tab",
  forks: "Allow forking this repository",
  watch: "Allow new watchers (existing watchers keep working)",
  star: "Allow new stars (existing stars are kept and counted)",
};

/** All-on: the zero-migration default (absent section, unset key). */
export function allFeatures() {
  return { issues: true, pulls: true, releases: true, forks: true, watch: true, star: true };
}

/**
 * Resolve a wire `features` value onto six booleans. Non-objects
 * (undefined/null — loading, deleted, pre-#522 server) resolve all-on;
 * only an explicit `false` disables a flag, so a partial or sloppy
 * object can never strand a tab hidden.
 */
export function resolveFeatures(features) {
  const out = allFeatures();
  if (!features || typeof features !== "object") return out;
  for (const k of FEATURE_KEYS) {
    if (features[k] === false) out[k] = false;
  }
  return out;
}

/**
 * isFeatureDisabled(summary, key) → boolean: the tab/pill gate. True
 * only when the shared summary explicitly carries `false` for the key —
 * loading, deleted, and pre-#522 summaries keep everything shown.
 */
export function isFeatureDisabled(summary, key) {
  const f = summary?.features;
  if (!f || typeof f !== "object") return false;
  return f[key] === false;
}

/**
 * Read the `[features]` section out of a settings TOML document for the
 * settings editor prefill. Returns `{key: true|false|null}` — null =
 * unset (renders checked: the server default is enabled). Only `true` /
 * `false` literals read; any other spelling stays null (the server
 * rejects it on save anyway, and the editor never writes it).
 */
export function extractFeatures(tomlText) {
  const out = {};
  for (const k of FEATURE_KEYS) out[k] = null;
  if (typeof tomlText !== "string" || tomlText === "") return out;
  const lines = tomlText.split("\n");
  let inFeatures = false;
  for (const line of lines) {
    const header = line.match(/^\s*\[([^\]]*)\]\s*(#.*)?$/);
    if (header) {
      inFeatures = header[1].trim() === "features";
      continue;
    }
    if (!inFeatures) continue;
    const m = line.match(/^\s*([A-Za-z0-9_]+)\s*=\s*(true|false)\s*(#.*)?$/);
    if (m && Object.hasOwn(out, m[1])) out[m[1]] = m[2] === "true";
  }
  return out;
}

/**
 * Return the document with the `[features]` section set to `flags`
 * (six explicit `key = true|false` lines, FEATURE_KEYS order). An
 * existing `[features]` block (header + body, incl. comments) is
 * replaced wholesale at its position; otherwise the canonical block is
 * appended. Every other section — including a top-level `description`
 * line — passes through untouched.
 */
export function withFeatures(tomlText, flags) {
  const resolved = resolveFeatures({ ...allFeatures(), ...(flags ?? {}) });
  const block = ["[features]"];
  for (const k of FEATURE_KEYS) block.push(`${k} = ${resolved[k] ? "true" : "false"}`);
  const text = typeof tomlText === "string" ? tomlText : "";
  if (text === "") return block.join("\n") + "\n";
  const lines = text.split("\n");
  let start = -1;
  let end = lines.length;
  for (let i = 0; i < lines.length; i++) {
    const header = lines[i].match(/^\s*\[([^\]]*)\]\s*(#.*)?$/);
    if (!header) continue;
    if (header[1].trim() === "features" && start === -1) {
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
    return text + sep + block.join("\n") + "\n";
  }
  return [...lines.slice(0, start), ...block, ...lines.slice(end)].join("\n");
}
