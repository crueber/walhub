// web/src/lib/collab.js — Feature 08 pure helpers (dependency-free,
// headless-testable per 12 §5): the §6 TTL table, the §4 frame→key
// mapping, and the SSE reconnect backoff. The data layer (data.js) and
// the stream helper (sse.js) consume these; unit tests import this
// module directly with no SolidJS install.

/** 08 §6 data-layer TTL table (ms; Infinity = immutable content). */
export const TTL = {
  repo: 5_000, // `repo:{o}/{r}` — GET …/api
  perms: 30_000, // `perms:{o}/{r}` — GET …/api/permissions
  issues: 5_000, // `issues:{o}/{r}:{filter}` — issue list windows
  pulls: 5_000, // `pulls:{o}/{r}:{filter}` — PR list windows
  issue: 5_000, // `issue:{o}/{r}:{num}` — thread headers
  pull: 5_000, // `pull:{o}/{r}:{num}` — PR headers
  events: Infinity, // `events:{o}/{r}:{num}:{after}:{n}` — immutable windows
  labels: 30_000, // `labels:{o}/{r}`
  milestones: 30_000, // `milestones:{o}/{r}`
  diff: Infinity, // `diff:{o}/{r}:{num}:{head}` — sha-addressed content
  prcommits: Infinity, // `prcommits:{o}/{r}:{num}:{head}`
  reviews: 5_000, // `reviews:{o}/{r}:{num}`
  threads: 5_000, // `threads:{o}/{r}:{num}`
  checks: 5_000, // `checks:{o}/{r}:{sha}` — mutable, no-store server-side
  checkindex: 5_000, // `checkindex:{o}/{r}:{after}`
  releases: 30_000, // `releases:{o}/{r}`
  release: 60_000, // `release:{o}/{r}:{tag}`
  social: 30_000, // `social:{o}/{r}`
  assignables: 300_000, // `assignables:{o}/{r}`
  notifications: 5_000, // `notifications:me`
};

/**
 * collabKeys(full, frame) → string[]: the 08 §4 frame table. One repo
 * stream frame fans out to the data-layer keys it invalidates; a
 * trailing "*" marks a prefix (list windows). Unknown kinds map to []
 * (forward compatible — a new publisher never breaks old pages).
 * `full` is "owner/name".
 */
export function collabKeys(full, frame) {
  const kind = frame?.kind;
  const num = frame?.num;
  switch (kind) {
    case "issue":
      return [`issue:${full}:${num}`, `issues:${full}:*`];
    case "issue_event":
      return [`issue:${full}:${num}`, `events:${full}:${num}:*`];
    case "pull":
      return [`pull:${full}:${num}`, `pulls:${full}:*`, `pulldiff:${full}:${num}`];
    case "review":
      return [`pull:${full}:${num}`, `reviews:${full}:${num}`];
    case "thread": {
      const keys = [`threads:${full}:${num}`];
      if (frame?.thread_id) keys.push(`thread:${num}:${frame.thread_id}`);
      return keys;
    }
    case "check": {
      const keys = [`checkindex:${full}:*`, `checks:${full}:*`];
      if (frame?.sha) keys.push(`checks:${full}:${frame.sha}`, `statuses:${full}:${frame.sha}`);
      return keys;
    }
    case "release": {
      const keys = [`releases:${full}:*`, `latest:${full}`];
      if (frame?.tag) keys.push(`release:${full}:${frame.tag}`);
      return keys;
    }
    case "access":
      return [`perms:${full}`, `access:${full}`, `collaborators:${full}`, `assignables:${full}`];
    default:
      return [];
  }
}

/**
 * backoffMs(attempt) — capped exponential reconnect backoff (08 §4):
 * 1 s → 30 s, reset on open. Attempt counts consecutive failures from 0.
 */
export function backoffMs(attempt) {
  const n = Math.max(0, Math.floor(attempt) || 0);
  return Math.min(30_000, 1_000 * 2 ** Math.min(n, 5));
}
