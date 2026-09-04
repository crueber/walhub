/**
 * Notifications client group (docs/features/06 §6–§7): per-user tray,
 * unread count, read/unread flips, the per-user SSE stream, repo watch
 * toggles, and repo webhook CRUD + ping + deliveries. Top-level calls
 * ride `/api/v1/notifications…`; repo calls ride `/{o}/{r}/api/…` (both
 * lanes via the browser-lane rewrite). Streaming uses the fetch-based
 * reader (never `EventSource`; the per-user stream is a browser-lane,
 * credentials-included stream).
 */
import { readSse } from "./sse.js";
import { ReposError } from "./errors.js";

/**
 * Attach the top-level notifications surface onto a client instance.
 * @param {import("./core.js").ReposClient} client client to extend
 */
export function attachNotifications(client) {
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  client.notifications = {
    /** Paged tray: `GET /api/v1/notifications?state=&after=&n=` (no-store). */
    list: (query = {}, opts) =>
      client._call(`/api/v1/notifications${qs(query)}`, { method: "GET", ...opts }),
    /** Unread count: `GET /api/v1/notifications/unread_count` (no-store). */
    unreadCount: (opts) =>
      client._call("/api/v1/notifications/unread_count", { method: "GET", ...opts }),
    /** Mark read: `POST /api/v1/notifications/{id}/read` → Notification. */
    markRead: (id, opts) =>
      client._call(`/api/v1/notifications/${encodeURIComponent(id)}/read`, { method: "POST", ...opts }),
    /** Mark unread: `POST /api/v1/notifications/{id}/unread` → Notification. */
    markUnread: (id, opts) =>
      client._call(`/api/v1/notifications/${encodeURIComponent(id)}/unread`, { method: "POST", ...opts }),
    /** Mark all read: `POST /api/v1/notifications/read_all` → `{updated}`. */
    markAllRead: (opts) =>
      client._call("/api/v1/notifications/read_all", { method: "POST", ...json({}), ...opts }),
    /**
     * Live tray: `GET /api/v1/notifications/stream` (SSE). Calls
     * `onNotification` per `notification` frame. Honors `opts.signal`
     * (the primary cancel path) and returns a cancellation function
     * (§1.6: the caller owns the signal, the SDK owns the reader).
     */
    stream: async (onNotification, opts = {}) => {
      const controller = client._controller(opts?.signal);
      const req = client._request("/api/v1/notifications/stream", {
        headers: { Accept: "text/event-stream" },
        sse: false,
      });
      const res = await client._send(req, controller);
      if (!res.ok) throw new ReposError(res.status, `stream failed`, req.url);
      readSse(res, (frame) => {
        if (frame?.event === "notification") onNotification?.(frame.data);
      }, { signal: controller.signal }).catch(() => {});
      return () => controller.abort();
    },
  };
}

/**
 * Attach the repo-scoped watch + webhooks surface onto a repo client.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachNotifyRepo(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.watch = {
    /** `GET …/watch` → `{watching, watchers}` (no-store). */
    get: (opts) => client._call(p("/watch"), { method: "GET", ...opts }),
    /** `PUT …/watch` → `{watching: true, watchers}`. */
    set: (on, opts) =>
      client._call(p("/watch"), { method: on === false ? "DELETE" : "PUT", ...opts }),
  };

  repo.webhooks = {
    /** `GET …/webhooks` → `{webhooks}` (admin; secrets never returned). */
    list: (opts) => client._call(p("/webhooks"), { method: "GET", ...opts }),
    /** `POST …/webhooks` → Hook (secret shown never; `secret_set` instead). */
    create: (spec = {}, opts) =>
      client._call(p("/webhooks"), { method: "POST", ...json(spec), ...opts }),
    /** `GET …/webhooks/{id}` → Hook (admin). */
    get: (id, opts) =>
      client._call(p(`/webhooks/${encodeURIComponent(id)}`), { method: "GET", ...opts }),
    /** `PATCH …/webhooks/{id}` → Hook (admin, CAS'd). */
    update: (id, patch = {}, opts) =>
      client._call(p(`/webhooks/${encodeURIComponent(id)}`), { method: "PATCH", ...json(patch), ...opts }),
    /** `DELETE …/webhooks/{id}` → 204 (also drops cursor + deliveries). */
    remove: (id, opts) =>
      client._call(p(`/webhooks/${encodeURIComponent(id)}`), { method: "DELETE", ...opts }),
    /** `POST …/webhooks/{id}/ping` → `{delivery}` (admin). */
    ping: (id, opts) =>
      client._call(p(`/webhooks/${encodeURIComponent(id)}/ping`), { method: "POST", ...opts }),
    /** `GET …/webhooks/{id}/deliveries` → last-25 ring (no-store). */
    deliveries: (id, opts) =>
      client._call(p(`/webhooks/${encodeURIComponent(id)}/deliveries`), { method: "GET", ...opts }),
  };
}

function qs(params) {
  const parts = [];
  for (const [k, v] of Object.entries(params ?? {})) {
    if (v === undefined || v === null || v === "") continue;
    parts.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
  }
  return parts.length ? `?${parts.join("&")}` : "";
}

export { readSse };
