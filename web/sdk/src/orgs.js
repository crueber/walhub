/**
 * Orgs client group (features/01 §8–§9): orgs, members, teams, org invites.
 * `client.orgs.*` over `/api/v1/orgs[/…]` (both lanes).
 */

const enc = encodeURIComponent;

/**
 * Attach the org surface onto a ReposClient instance.
 * @param {import("./core.js").ReposClient} client client to extend
 */
export function attachOrgs(client) {
  const call = (path, opts) => client._call(path, { sse: false, ...opts });
  const orgPath = (org, suffix = "") => `/api/v1/orgs/${enc(org.toLowerCase())}${suffix}`;

  client.orgs = {
    /** Sorted org names. */
    list: (opts) => call("/api/v1/orgs", { method: "GET", ...opts }),
    /** Create; creator becomes owner. `{org, display_name?, description?}` → `{org}`. */
    create: (body, opts) =>
      call("/api/v1/orgs", {
        method: "POST",
        body: JSON.stringify(body ?? {}),
        headers: { "Content-Type": "application/json" },
        ...opts,
      }),
    /** Org profile; null when unknown. */
    get: async (org, opts) => {
      try {
        return await call(orgPath(org), { method: "GET", ...opts });
      } catch (err) {
        if (err?.status === 404) return null;
        throw err;
      }
    },
    /** Edit profile. */
    put: (org, body, opts) =>
      call(orgPath(org), {
        method: "PUT",
        body: JSON.stringify(body ?? {}),
        headers: { "Content-Type": "application/json" },
        ...opts,
      }),
    /** Delete (owner; 409 while the org owns repos). */
    delete: (org, opts) => call(orgPath(org), { method: "DELETE", ...opts }),

    members: {
      /** Roster. */
      list: (org, opts) => call(orgPath(org, "/members"), { method: "GET", ...opts }),
      /** One row; null when not a member. */
      get: async (org, principal, opts) => {
        try {
          return await call(orgPath(org, `/members/${enc(principal.toLowerCase())}`), { method: "GET", ...opts });
        } catch (err) {
          if (err?.status === 404) return null;
          throw err;
        }
      },
      /** Add/re-role: `{role}`. */
      put: (org, principal, role, opts) =>
        call(orgPath(org, `/members/${enc(principal.toLowerCase())}`), {
          method: "PUT",
          body: JSON.stringify({ role }),
          headers: { "Content-Type": "application/json" },
          ...opts,
        }),
      /** Remove (last owner → 409). */
      delete: (org, principal, opts) =>
        call(orgPath(org, `/members/${enc(principal.toLowerCase())}`), { method: "DELETE", ...opts }),
    },

    teams: {
      /** Team list (`?n=` page, default 100). */
      list: (org, query = {}, opts) => {
        const q = query.n != null ? `?n=${encodeURIComponent(query.n)}` : "";
        return call(orgPath(org, `/teams${q}`), { method: "GET", ...opts });
      },
      /** Create: `{slug, name?, description?}`. */
      create: (org, body, opts) =>
        call(orgPath(org, "/teams"), {
          method: "POST",
          body: JSON.stringify(body ?? {}),
          headers: { "Content-Type": "application/json" },
          ...opts,
        }),
      /** One team; null when unknown. */
      get: async (org, slug, opts) => {
        try {
          return await call(orgPath(org, `/teams/${enc(slug.toLowerCase())}`), { method: "GET", ...opts });
        } catch (err) {
          if (err?.status === 404) return null;
          throw err;
        }
      },
      /** Edit name/description. */
      put: (org, slug, body, opts) =>
        call(orgPath(org, `/teams/${enc(slug.toLowerCase())}`), {
          method: "PUT",
          body: JSON.stringify(body ?? {}),
          headers: { "Content-Type": "application/json" },
          ...opts,
        }),
      /** Delete (strips bindings from referencing access.json files). */
      delete: (org, slug, opts) => call(orgPath(org, `/teams/${enc(slug.toLowerCase())}`), { method: "DELETE", ...opts }),
      /** Add a member. */
      addMember: (org, slug, principal, opts) =>
        call(orgPath(org, `/teams/${enc(slug.toLowerCase())}/members/${enc(principal.toLowerCase())}`), {
          method: "PUT",
          ...opts,
        }),
      /** Remove a member. */
      removeMember: (org, slug, principal, opts) =>
        call(orgPath(org, `/teams/${enc(slug.toLowerCase())}/members/${enc(principal.toLowerCase())}`), {
          method: "DELETE",
          ...opts,
        }),
    },

    invites: {
      /** Pending org invites (owner). */
      list: (org, opts) => call(orgPath(org, "/invitations"), { method: "GET", ...opts }),
      /** Invite: `{email, role}` → `{id, accept_url}`. */
      create: (org, body, opts) =>
        call(orgPath(org, "/invitations"), {
          method: "POST",
          body: JSON.stringify(body ?? {}),
          headers: { "Content-Type": "application/json" },
          ...opts,
        }),
      /** Cancel (owner). */
      cancel: (org, id, opts) => call(orgPath(org, `/invitations/${enc(id)}`), { method: "DELETE", ...opts }),
    },
  };
}
