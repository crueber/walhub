/**
 * submitKeys.js — shared Cmd/Ctrl+Enter submit helper (Forgejo #450).
 *
 * ONE helper for every full-sized (form-submitting) textarea in the SPA:
 * Cmd/Ctrl+Enter triggers the same submit the form's save button performs.
 * Per-page copies are the bug this module retires — import it instead.
 *
 * The helper takes a CALLBACK, not a form: three surfaces (Org bio,
 * Settings push-policy, Settings wal.toml bundles) save via onClick
 * handlers, not form submits, so `submitFn` is invoked directly with the
 * keydown event (form handlers call `e.preventDefault()` on it, harmlessly
 * repeated; no-arg callbacks simply ignore the extra argument).
 *
 * Contract (the issue's edge-case guards):
 * - Fires on (metaKey || ctrlKey) && key === "Enter" only — Mac + Win/Linux.
 * - Plain Enter (no modifier) is untouched: the newline inserts natively,
 *   zero behavior change.
 * - Shift+Cmd/Ctrl+Enter does NOT submit (reserved for future soft-submit).
 * - No-op while busy/disabled: each caller passes its guard as
 *   `opts.isBusy` (e.g. `() => getBusy()`), so the keyboard path never
 *   double-submits against a disabled button.
 * - Autogrow coexistence: only the exact combo is swallowed
 *   (preventDefault + stopPropagation); every other key — including plain
 *   Enter — passes through, so initAutogrow/growTextarea behavior is
 *   unchanged.
 *
 * Pure module (no Solid, no DOM): headless-testable in Node like
 * lib/autogrow.js and lib/settingsNav.js. Setup.jsx is deliberately
 * excluded — its toml fields are small wizard inputs whose save is the
 * wizard's own "Test & Save" flow, and Enter-to-submit there would be
 * surprising.
 */

/**
 * Build an onKeyDown handler that submits on Cmd/Ctrl+Enter.
 *
 * @param {(e: KeyboardEvent) => void} submitFn — the save action; receives
 *   the swallowed keydown event (form handlers preventDefault it again).
 * @param {{ isBusy?: () => boolean }} [opts] — caller busy/disabled guard.
 * @returns {(e: KeyboardEvent) => void} the textarea onKeyDown handler.
 */
export function onSubmitKeys(submitFn, opts = {}) {
  const isBusy = opts.isBusy;
  return (e) => {
    if (!e || typeof submitFn !== "function") return;
    if (e.key !== "Enter") return;
    if (!e.metaKey && !e.ctrlKey) return; // plain Enter: native newline
    if (e.shiftKey) return; // Shift+combo: reserved, explicitly excluded
    if (typeof isBusy === "function" && isBusy()) return; // busy: no double-submit
    e.preventDefault();
    if (typeof e.stopPropagation === "function") e.stopPropagation();
    submitFn(e);
  };
}
