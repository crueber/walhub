// web/src/lib/label-colors.js — label color palette + validation (issue #325).
//
// Pure constants + pure helpers (no Solid, no DOM) so node --test covers
// them. Colors are 6-hex RGB without `#`, the same contract the labels
// create form posts (docs/features/02_issues.md §3.1 shapes — the server
// leaves color values unconstrained; this client-side palette is the only
// curation, so a future edit/rename flow can reuse it as-is).

/**
 * 8 preset swatches for the label create form — the classic GitHub label
 * palette hues (red/orange/yellow/green/blue/purple/magenta/gray), familiar
 * and legible on both themes. Lowercase 6-hex, no `#`. `d73a4a` stays first:
 * it is the create form's long-standing default.
 */
export const LABEL_COLOR_PRESETS = [
  "d73a4a",
  "e36209",
  "fbca04",
  "0e8a16",
  "1d76db",
  "5319e7",
  "d876e3",
  "3a3a3a",
];

/** The create form's default color (also LABEL_COLOR_PRESETS[0]). */
export const DEFAULT_LABEL_COLOR = "d73a4a";

const HEX6_RE = /^[0-9a-fA-F]{6}$/;

/**
 * isValidLabelColor(v) → bool: true for exactly 6 hex digits (no `#`,
 * no whitespace) — the same rule the form's `pattern` attribute enforces
 * on submit. Only valid values ever drive a preview swatch.
 */
export function isValidLabelColor(v) {
  return typeof v === "string" && HEX6_RE.test(v);
}

/**
 * isPresetLabelColor(v) → bool: case-insensitive membership in
 * LABEL_COLOR_PRESETS. Decides whether the custom-hex input stays
 * revealed for a non-preset effective value.
 */
export function isPresetLabelColor(v) {
  if (typeof v !== "string") return false;
  const lower = v.toLowerCase();
  return LABEL_COLOR_PRESETS.some((p) => p === lower);
}
