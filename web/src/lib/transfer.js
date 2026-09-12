// web/src/lib/transfer.js — repo-transfer body helper (Forgejo #358).
// Headless-testable: no Solid, no DOM — importable in Node.
//
// The server is authoritative (repo admin on the source + #346 admission
// on the destination; 401/403/404/409 verbatim); this helper only shapes
// the client body: owner is trimmed ("" = unset, the server 400s), an
// empty repo is omitted so the server defaults to the current name
// (transfer+rename passes both).

/**
 * transferBody(form) → the SDK POST body: `{owner}` plus `repo` only
 * when non-empty. Non-objects and non-strings coerce to "" (inputs stay
 * controlled, like orgCreateBody).
 */
export function transferBody(form) {
  const src = form && typeof form === "object" ? form : {};
  const str = (v) => (typeof v === "string" ? v : "");
  const owner = str(src.owner).trim();
  const repo = str(src.repo).trim();
  const body = { owner };
  if (repo) body.repo = repo;
  return body;
}
