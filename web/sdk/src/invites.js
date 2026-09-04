/**
 * Invites client group (features/01 §7–§9): top-level inbox + repo invites.
 * `client.invites.mine/list/accept/cancel` and `repo.invites.*`.
 */

const enc = encodeURIComponent;

/**
 * Attach the top-level invite surface onto a ReposClient instance.
 * @param {import("./core.js").ReposClient} client client to extend
 */
export function attachInvites(client) {
  const call = (path, opts) => client._call(path, { sse: false, ...opts });
  client.invites = {
    /** My pending invites. */
    mine: (opts) => call("/api/v1/invitations", { method: "GET", ...opts }),
    /** Signed-link preview (`?token=`); the token is redacted server-side. */
    get: (id, token, opts) => {
      const q = token ? `?token=${enc(token)}` : "";
      return call(`/api/v1/invitations/${enc(id)}${q}`, { method: "GET", ...opts });
    },
    /** Accept (subject match) → `{bound: "org"|"repo"}`. */
    accept: (id, opts) => call(`/api/v1/invitations/${enc(id)}/accept`, { method: "POST", ...opts }),
    /** Decline (invitee) / cancel (inviter). */
    cancel: (id, opts) => call(`/api/v1/invitations/${enc(id)}`, { method: "DELETE", ...opts }),
  };
}

/**
 * Attach the repo-scoped invite surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachRepoInvites(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);

  repo.invites = {
    /** Pending invites (admin). */
    list: (opts) => client._call(p("/invitations"), { method: "GET", ...opts }),
    /** Invite: `{subject, role}` → `{id, accept_url}` (admin). */
    create: (body, opts) =>
      client._call(p("/invitations"), {
        method: "POST",
        body: JSON.stringify(body ?? {}),
        headers: { "Content-Type": "application/json" },
        ...opts,
      }),
    /** Cancel (admin). */
    cancel: (id, opts) => client._call(p(`/invitations/${enc(id)}`), { method: "DELETE", ...opts }),
  };
}
