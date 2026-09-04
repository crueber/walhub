// web/src/lib/reactions.js — reaction content ↔ emoji presentation (issue #31).
//
// Reaction CONTENTS are wire contracts (docs/features/02 §8): "+1", "-1",
// "laugh", "hooray", "confused", "heart", "rocket", "eyes" — the API, the
// bucket summary keys, and the accessible labels all use these spellings.
// What the eye sees is a unicode glyph instead (no emoji library per the
// frontend dependency budget, AGENTS.md law 1). Headless (no Solid, no DOM)
// so node --test covers it.

export const REACTIONS = ["+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"];

export const REACTION_EMOJI = {
  "+1": "👍",
  "-1": "👎",
  laugh: "😄",
  hooray: "🎉",
  confused: "😕",
  heart: "❤️",
  rocket: "🚀",
  eyes: "👀",
};

/** Glyph for a reaction content; unknown contents pass through unchanged. */
export function reactionEmoji(content) {
  return REACTION_EMOJI[content] ?? content;
}

// reaction_summary keys are %06x event seqs (02 §1.1: "000003", never "3").
export function seqKey(seq) {
  return Number(seq).toString(16).padStart(6, "0");
}

/**
 * summaryEntries(summary, seq) → [[content, count], ...]: the nonzero
 * counts for one event, in REACTIONS order (unknown contents appended
 * after, in first-seen order). A pure view of thread.reaction_summary.
 */
export function summaryEntries(summary, seq) {
  const per = (summary ?? {})[seqKey(seq)] ?? {};
  const out = [];
  for (const r of REACTIONS) {
    if ((per[r] ?? 0) > 0) out.push([r, per[r]]);
  }
  for (const k of Object.keys(per)) {
    if (!REACTIONS.includes(k) && (per[k] ?? 0) > 0) out.push([k, per[k]]);
  }
  return out;
}

/**
 * Timeline row text for a reaction_changed event: the glyph up front for
 * scanners, the word form kept for clarity ("reacted 👀 eyes on #0").
 */
export function reactionChangedText(ev) {
  const verb = ev.op === "remove" ? "unreacted" : "reacted";
  return `${verb} ${reactionEmoji(ev.content)} ${ev.content} on #${ev.target_event_seq}`;
}
