/**
 * Orgs client group (features/01 §8–§9): orgs, members, teams, org invites.
 * `client.orgs.*` over `/api/v1/orgs[/…]` (both lanes), incl. org webhooks.
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

    avatar: {
      /** Avatar image path (`v` = avatar_updated_at cache-busts the
       *  immutable max-age response). Callers gate on the org doc's
       *  avatar_content_type — the client never probes the bytes. */
      url: (org, v) => `${orgPath(org, "/avatar")}${v ? `?v=${enc(v)}` : ""}`,
      /** Upload raw image bytes (PUT, owner): PNG/JPEG/GIF/WebP, 2 MiB cap. */
      upload: async (org, data, { contentType, ...opts } = {}) => {
        const bytes = data instanceof Uint8Array ? data : new Uint8Array(await toArrayBuffer(data));
        return call(orgPath(org, "/avatar"), {
          method: "PUT",
          headers: { "Content-Type": contentType ?? "application/octet-stream" },
          body: bytes,
          ...opts,
        });
      },
      /** Remove the avatar (DELETE, owner). */
      remove: (org, opts) => call(orgPath(org, "/avatar"), { method: "DELETE", ...opts }),
    },

    webhooks: {
      /** Org hook list: `GET …/webhooks` → `{webhooks}` (owner; secrets never returned). */
      list: (org, opts) => call(orgPath(org, "/webhooks"), { method: "GET", ...opts }),
      /** Create: `{url, events?, secret?, active?, insecure_tls?}` → Hook (owner). */
      create: (org, spec = {}, opts) =>
        call(orgPath(org, "/webhooks"), {
          method: "POST",
          body: JSON.stringify(spec ?? {}),
          headers: { "Content-Type": "application/json" },
          ...opts,
        }),
      /** One hook; null when unknown. */
      get: async (org, id, opts) => {
        try {
          return await call(orgPath(org, `/webhooks/${enc(id)}`), { method: "GET", ...opts });
        } catch (err) {
          if (err?.status === 404) return null;
          throw err;
        }
      },
      /** Update: `{url?, events?, secret?, active?, insecure_tls?}` → Hook (owner, CAS'd). */
      update: (org, id, patch = {}, opts) =>
        call(orgPath(org, `/webhooks/${enc(id)}`), {
          method: "PATCH",
          body: JSON.stringify(patch ?? {}),
          headers: { "Content-Type": "application/json" },
          ...opts,
        }),
      /** Delete: drops the config, both cursor families, and the deliveries ring. */
      remove: (org, id, opts) => call(orgPath(org, `/webhooks/${enc(id)}`), { method: "DELETE", ...opts }),
      /** Ping: `POST …/webhooks/{id}/ping` → `{delivery}` (owner). */
      ping: (org, id, opts) =>
        call(orgPath(org, `/webhooks/${enc(id)}/ping`), { method: "POST", ...opts }),
      /** Deliveries: `GET …/webhooks/{id}/deliveries` → last-25 ring (no-store). */
      deliveries: (org, id, opts) =>
        call(orgPath(org, `/webhooks/${enc(id)}/deliveries`), { method: "GET", ...opts }),
    },

    members: {      /** Roster. */
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

/** Coerce upload input to bytes (File/Blob preferred — carries image data). */
async function toArrayBuffer(data) {
  if (data instanceof ArrayBuffer) return data;
  if (data?.buffer instanceof ArrayBuffer) return data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
  if (typeof data === "string") return new TextEncoder().encode(data).buffer;
  if (data?.arrayBuffer instanceof Function) return data.arrayBuffer();
  throw new Error("orgs.avatar.upload: data must be bytes, a string, or a Blob/File");
}
