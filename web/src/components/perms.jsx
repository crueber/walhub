// web/src/components/perms.jsx — 08 §5 permissions-driven UI helpers.
//
// The server is authoritative (P6); client gating is cosmetic — any 403
// surfaces its plain-text body in the error tray. The resolved role comes
// from repo.permissions() cached as `perms:{o}/{r}` (30 s TTL per 08 §6).
// Pages resolve gating off this one cached signal — no per-component
// refetch. Hide entirely when the role is absent; render disabled with a
// title tooltip when the role is present but object state forbids.

import { useData } from "../lib/data.js";
import { TTL } from "../lib/collab.js";

const LADDER = ["read", "triage", "write", "maintain", "admin"];

/** Rank a role (null/unknown → -1, below every real role). */
export function roleRank(role) {
  return LADDER.indexOf(role ?? "");
}

/** True when role reaches want on the P6 ladder. */
export function roleAtLeast(role, want) {
  return roleRank(role) >= roleRank(want);
}

/**
 * useRole(full, client) → { role, loading }: the cached resolved role
 * for this repo ("read"|…|"admin"|null). Anonymous resolves null (or
 * read when anonymous_read admits them) — server-side, per 08 §5.
 */
export function useRole(full, client) {
  const key = () => `perms:${typeof full === "function" ? full() : full}`;
  const [getView] = useData(
    key,
    () => client.permissions().catch(() => ({ role: null })),
    TTL.perms,
  );
  const role = () => getView()?.role ?? null;
  return { role, loading: () => getView() === undefined };
}
