// web/src/lib/label-packs.js — built-in label packs for the empty labels
// page (issue #324; docs/features/02_issues.md §3.1 shapes).
//
// Pure constants + pure helpers (no Solid, no DOM) so node --test covers
// them. Colors are 6-hex RGB without `#`; names/descriptions satisfy the
// server validation (name 1–64 chars, unique case-insensitively,
// description ≤ 200 chars).
//
// Sources, verified 2026-09-11 against the live services:
// - GitHub: `GET https://api.github.com/repos/albandil/Hex/labels` — the 8
//   entries flagged `"default": true` on a pristine repo. Note: the classic
//   `documentation` (#0075ca) is NOT among GitHub's current defaults, so
//   this pack omits it rather than shipping a stale 9-label memory.
// - GitLab: "Generate a default set of labels"
//   (docs.gitlab.com → Manage > Labels lists bug, confirmed, critical,
//   discussion, documentation, enhancement, suggestion, support) with colors
//   from `lib/gitlab/issues_labels.rb` @ master (gitlab-org/gitlab):
//   red #d9534f (bug, critical, confirmed), yellow #f0ad4e (documentation,
//   support), blue #428bca (discussion, suggestion), green #5cb85c
//   (enhancement). GitLab generates no descriptions, so entries carry none.

/** GitHub's current default label set (8 labels, live-verified). */
export const GITHUB_LABEL_PACK = {
  id: "github",
  title: "GitHub defaults",
  labels: [
    { name: "bug", color: "d73a4a", description: "Something isn't working" },
    { name: "duplicate", color: "cfd3d7", description: "This issue or pull request already exists" },
    { name: "enhancement", color: "a2eeef", description: "New feature or request" },
    { name: "good first issue", color: "7057ff", description: "Good for newcomers" },
    { name: "help wanted", color: "008672", description: "Extra attention is needed" },
    { name: "invalid", color: "e4e669", description: "This doesn't seem right" },
    { name: "question", color: "d876e3", description: "Further information is requested" },
    { name: "wontfix", color: "ffffff", description: "This will not be worked on" },
  ],
};

/** GitLab's "Generate a default set of labels" set (8 labels, live-verified). */
export const GITLAB_LABEL_PACK = {
  id: "gitlab",
  title: "GitLab defaults",
  labels: [
    { name: "bug", color: "d9534f" },
    { name: "critical", color: "d9534f" },
    { name: "confirmed", color: "d9534f" },
    { name: "documentation", color: "f0ad4e" },
    { name: "support", color: "f0ad4e" },
    { name: "discussion", color: "428bca" },
    { name: "suggestion", color: "428bca" },
    { name: "enhancement", color: "5cb85c" },
  ],
};

/** Both packs, in display order. */
export const LABEL_PACKS = [GITHUB_LABEL_PACK, GITLAB_LABEL_PACK];

/**
 * missingFromPack(packLabels, existingNames) → entries of `packLabels`
 * whose name is not already present in `existingNames`
 * (case-insensitive — label names are unique case-insensitively per
 * 02 §3.1). Adding a pack skips these instead of failing the whole pack,
 * so re-adding is idempotent-ish.
 */
export function missingFromPack(packLabels, existingNames) {
  const have = new Set((existingNames ?? []).map((n) => String(n).toLowerCase()));
  return (packLabels ?? []).filter((l) => !have.has(String(l.name).toLowerCase()));
}
