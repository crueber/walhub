import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/** Drive one SDK call against a scripted fetch; assert method+path. */
async function drive(run, { status = 200, body = {} } = {}) {
  const calls = [];
  const fetch = async (url, init = {}) => {
    calls.push({ url: String(url).replace(BASE, ""), method: init.method, body: init.body });
    return jsonResponse(body, { status });
  };
  const client = new ReposClient({ base: BASE, fetch, token: "t" }); // bearer lane → paths unchanged
  const out = await run(client);
  return { seen: calls[0], out };
}

test("identity SDK surface: users/orgs/access/invites paths", async () => {
  const cases = [
    { name: "users.get", run: (c) => c.users.get("Jane@X.c"), method: "GET", path: "/api/v1/users/jane%40x.c" },
    { name: "users.put", run: (c) => c.users.put("a@b.c", { display_name: "A" }), method: "PUT", path: "/api/v1/users/a%40b.c" },
    { name: "orgs.list", run: (c) => c.orgs.list(), method: "GET", path: "/api/v1/orgs" },
    { name: "orgs.create", run: (c) => c.orgs.create({ org: "acme" }), method: "POST", path: "/api/v1/orgs" },
    { name: "orgs.get", run: (c) => c.orgs.get("acme"), method: "GET", path: "/api/v1/orgs/acme" },
    { name: "orgs.put", run: (c) => c.orgs.put("acme", {}), method: "PUT", path: "/api/v1/orgs/acme" },
    { name: "orgs.delete", run: (c) => c.orgs.delete("acme"), method: "DELETE", path: "/api/v1/orgs/acme" },
    { name: "orgs.members.list", run: (c) => c.orgs.members.list("acme"), method: "GET", path: "/api/v1/orgs/acme/members" },
    { name: "orgs.members.put", run: (c) => c.orgs.members.put("acme", "a@b.c", "member"), method: "PUT", path: "/api/v1/orgs/acme/members/a%40b.c" },
    { name: "orgs.members.delete", run: (c) => c.orgs.members.delete("acme", "a@b.c"), method: "DELETE", path: "/api/v1/orgs/acme/members/a%40b.c" },
    { name: "orgs.teams.list", run: (c) => c.orgs.teams.list("acme", { n: 10 }), method: "GET", path: "/api/v1/orgs/acme/teams?n=10" },
    { name: "orgs.teams.create", run: (c) => c.orgs.teams.create("acme", { slug: "s" }), method: "POST", path: "/api/v1/orgs/acme/teams" },
    { name: "orgs.teams.get", run: (c) => c.orgs.teams.get("acme", "s"), method: "GET", path: "/api/v1/orgs/acme/teams/s" },
    { name: "orgs.teams.put", run: (c) => c.orgs.teams.put("acme", "s", {}), method: "PUT", path: "/api/v1/orgs/acme/teams/s" },
    { name: "orgs.teams.delete", run: (c) => c.orgs.teams.delete("acme", "s"), method: "DELETE", path: "/api/v1/orgs/acme/teams/s" },
    { name: "orgs.teams.addMember", run: (c) => c.orgs.teams.addMember("acme", "s", "a@b.c"), method: "PUT", path: "/api/v1/orgs/acme/teams/s/members/a%40b.c" },
    { name: "orgs.teams.removeMember", run: (c) => c.orgs.teams.removeMember("acme", "s", "a@b.c"), method: "DELETE", path: "/api/v1/orgs/acme/teams/s/members/a%40b.c" },
    { name: "orgs.invites.list", run: (c) => c.orgs.invites.list("acme"), method: "GET", path: "/api/v1/orgs/acme/invitations" },
    { name: "orgs.invites.create", run: (c) => c.orgs.invites.create("acme", {}), method: "POST", path: "/api/v1/orgs/acme/invitations" },
    { name: "orgs.invites.cancel", run: (c) => c.orgs.invites.cancel("acme", "id1"), method: "DELETE", path: "/api/v1/orgs/acme/invitations/id1" },
    { name: "invites.mine", run: (c) => c.invites.mine(), method: "GET", path: "/api/v1/invitations" },
    { name: "invites.get", run: (c) => c.invites.get("id1", "tok"), method: "GET", path: "/api/v1/invitations/id1?token=tok" },
    { name: "invites.accept", run: (c) => c.invites.accept("id1"), method: "POST", path: "/api/v1/invitations/id1/accept" },
    { name: "invites.cancel", run: (c) => c.invites.cancel("id1"), method: "DELETE", path: "/api/v1/invitations/id1" },
    { name: "repo.access.get", run: (c) => c.repo("o/r").access.get(), method: "GET", path: "/o/r/api/access" },
    { name: "repo.access.put", run: (c) => c.repo("o/r").access.put({ version: 1 }), method: "PUT", path: "/o/r/api/access" },
    { name: "repo.invites.list", run: (c) => c.repo("o/r").invites.list(), method: "GET", path: "/o/r/api/invitations" },
    { name: "repo.invites.create", run: (c) => c.repo("o/r").invites.create({}), method: "POST", path: "/o/r/api/invitations" },
    { name: "repo.invites.cancel", run: (c) => c.repo("o/r").invites.cancel("id1"), method: "DELETE", path: "/o/r/api/invitations/id1" },
  ];
  for (const tc of cases) {
    const { seen } = await drive(tc.run);
    assert.equal(seen.method, tc.method, `${tc.name} method`);
    assert.equal(seen.url, tc.path, `${tc.name} path`);
  }
});

test("identity SDK: 404 getters resolve null", async () => {
  for (const run of [
    (c) => c.users.get("ghost@x.c"),
    (c) => c.orgs.get("ghost"),
    (c) => c.orgs.members.get("acme", "ghost@x.c"),
    (c) => c.orgs.teams.get("acme", "ghost"),
  ]) {
    const { out } = await drive(run, { status: 404, body: "not found" });
    assert.equal(out, null);
  }
});
