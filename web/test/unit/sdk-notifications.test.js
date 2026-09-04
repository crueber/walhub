import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse, sseResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";
const ID = "f".repeat(32);

/** Notifications/watch/webhooks surface (docs/features/06 §6–§7). */
const SURFACE = [
  { name: "notifications.list", run: (c) => c.notifications.list({ state: "unread", n: 10 }), method: "GET", path: "/api/v1/notifications?state=unread&n=10" },
  { name: "notifications.unreadCount", run: (c) => c.notifications.unreadCount(), method: "GET", path: "/api/v1/notifications/unread_count" },
  { name: "notifications.markRead", run: (c) => c.notifications.markRead(ID), method: "POST", path: `/api/v1/notifications/${ID}/read` },
  { name: "notifications.markUnread", run: (c) => c.notifications.markUnread(ID), method: "POST", path: `/api/v1/notifications/${ID}/unread` },
  { name: "notifications.markAllRead", run: (c) => c.notifications.markAllRead(), method: "POST", path: "/api/v1/notifications/read_all" },
  { name: "watch.get", run: (c) => c.repo("o/r").watch.get(), method: "GET", path: "/o/r/api/watch" },
  { name: "watch.set on", run: (c) => c.repo("o/r").watch.set(true), method: "PUT", path: "/o/r/api/watch" },
  { name: "watch.set off", run: (c) => c.repo("o/r").watch.set(false), method: "DELETE", path: "/o/r/api/watch" },
  { name: "webhooks.list", run: (c) => c.repo("o/r").webhooks.list(), method: "GET", path: "/o/r/api/webhooks" },
  { name: "webhooks.create", run: (c) => c.repo("o/r").webhooks.create({ url: "https://h.example/x" }), method: "POST", path: "/o/r/api/webhooks" },
  { name: "webhooks.get", run: (c) => c.repo("o/r").webhooks.get("abc"), method: "GET", path: "/o/r/api/webhooks/abc" },
  { name: "webhooks.update", run: (c) => c.repo("o/r").webhooks.update("abc", { active: false }), method: "PATCH", path: "/o/r/api/webhooks/abc" },
  { name: "webhooks.remove", run: (c) => c.repo("o/r").webhooks.remove("abc"), method: "DELETE", path: "/o/r/api/webhooks/abc" },
  { name: "webhooks.ping", run: (c) => c.repo("o/r").webhooks.ping("abc"), method: "POST", path: "/o/r/api/webhooks/abc/ping" },
  { name: "webhooks.deliveries", run: (c) => c.repo("o/r").webhooks.deliveries("abc"), method: "GET", path: "/o/r/api/webhooks/abc/deliveries" },
];

test("notifications surface: every member hits its exact endpoint and method", async () => {
  for (const row of SURFACE) {
    const { fetch, calls } = fakeFetch((ctx) =>
      ctx.init.method === row.method ? jsonResponse({ ok: true }) : new Response("bad method", { status: 405 })
    );
    const client = new ReposClient({ base: BASE, fetch, token: "t" }); // bearer lane → paths unchanged
    await row.run(client);
    assert.equal(calls.length, 1, row.name);
    assert.equal(calls[0].url, `${BASE}${row.path}`, `${row.name} → ${row.method} ${row.path}`);
    assert.equal(calls[0].init.method, row.method, row.name);
  }
});

test("webhooks.create sends the spec payload", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ id: "abc" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").webhooks.create({ url: "https://h.example/x", events: ["commented"], secret: "s" });
  assert.equal(calls.length, 1);
  const body = JSON.parse(calls[0].init.body);
  assert.equal(body.url, "https://h.example/x");
  assert.deepEqual(body.events, ["commented"]);
  assert.equal(body.secret, "s");
});

test("notifications.stream delivers notification frames and cancels", async () => {
  const frames = [
    ": walgit\n\n",
    'event: notification\ndata: {"id":"abc","reason":"mentioned"}\n\n',
  ];
  const { fetch } = fakeFetch(() => sseResponse(frames));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  const seen = [];
  const cancel = await client.notifications.stream((n) => seen.push(n));
  await new Promise((r) => setTimeout(r, 50));
  assert.equal(seen.length, 1);
  assert.equal(seen[0].id, "abc");
  await cancel();
});
