// web/src/lib/sha.jsx — tiny SHA presentation helpers shared by the commits
// list and the commit detail (issue #25): truncated display, full-SHA copy.
//
// JSX, Solid-only, no data fetching: the copy state lives in the button.

import { createSignal, onCleanup, Show } from "solid-js";

/** First n hex chars (12 balances density against collision-indexing). */
export function shortSha(sha, n = 12) {
  return String(sha ?? "").slice(0, n);
}

async function copyText(text) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  // Non-secure contexts (plain http over the LAN): execCommand fallback.
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try {
    if (!document.execCommand("copy")) throw new Error("copy refused");
  } finally {
    ta.remove();
  }
}

/** Copy affordance for a full SHA: icon → ✓ for a beat, title names the SHA. */
export function CopySha(props) {
  const [getCopied, setCopied] = createSignal(false);
  let timer = 0;
  onCleanup(() => clearTimeout(timer));
  const copy = async (e) => {
    e.preventDefault();
    e.stopPropagation();
    try {
      await copyText(String(props.sha ?? ""));
    } catch {
      return; // clipboard unavailable: leave the glyph, do no harm
    }
    setCopied(true);
    clearTimeout(timer);
    timer = setTimeout(() => setCopied(false), 1200);
  };
  return (
    <button
      type="button"
      class="copy-sha muted flex shrink-0 cursor-pointer items-center rounded px-1 text-xs hover:text-zinc-900 dark:hover:text-zinc-100"
      title={getCopied() ? "copied!" : `copy full SHA ${String(props.sha ?? "")}`}
      aria-label={getCopied() ? "copied!" : `copy full SHA ${shortSha(props.sha)}`}
      aria-live="polite"
      onClick={copy}
    >
      <Show
        when={getCopied()}
        fallback={
          // Inline SVG (not a font glyph): U+29C9-style ⧉ is tofu on systems
          // without a symbol font, and this button must read everywhere.
          <svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">
            <rect x="5.5" y="5.5" width="8" height="8" rx="1.5" />
            <path d="M10.5 5.5v-2A1.5 1.5 0 0 0 9 2H4a1.5 1.5 0 0 0-1.5 1.5v5A1.5 1.5 0 0 0 4 10h1.5" />
          </svg>
        }
      >
        <span aria-hidden="true" class="font-mono text-emerald-600 dark:text-emerald-400">✓</span>
      </Show>
    </button>
  );
}
