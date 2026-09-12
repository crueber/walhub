import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { normalizeSource } from "../../sdk/src/import.js";
import { fakeFetch, jsonResponse, sseResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/** Imports surface (docs/features/10 §5): exact endpoints + methods. */
const SURFACE = [
  { name: "imports.start", run: (c) => c.imports.start({ source_url: "acme/r", owner: "acme", name: "r" }), method: "POST", path: "/api/v1/repos/imports" },
  { name: "imports.get", run: (c) => c.imports.get("i-1"), method: "GET", path: "/api/v1/repos/imports/i-1" },
];

test("imports surface: every member hits its exact endpoint and method", async () => {
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

test("imports.start sends a JSON body", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ task: { id: "i-1" }, target: "acme/r" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.imports.start({ source_url: "acme/r", owner: "acme", name: "r", token: "s3cr3t" });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.source_url, "acme/r");
  assert.equal(sent.owner, "acme");
  assert.equal(sent.token, "s3cr3t");
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
});

test("imports.attach resolves the terminal result and forwards progress", async () => {
  const chunks = [
    'event: notice\ndata: {"text":"clone start"}\n\n',
    'event: progress\ndata: {"label":"clone","done":1,"total":2,"unit":"objects","percent":50}\n\n',
    'event: result\ndata: {"repo":"acme/r","source_url":"https://github.com/acme/r.git","head_shas":{},"format":"sha1","imported_at":"2026-09-04T00:00:00Z"}\n\n',
  ];
  const { fetch } = fakeFetch(() => sseResponse(chunks));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  const seen = [];
  const result = await client.imports.attach("i-1", (p) => seen.push(p));
  assert.equal(result.repo, "acme/r");
  assert.equal(result.source_url, "https://github.com/acme/r.git");
  assert.equal(seen.length, 2);
  assert.equal(seen[0].event, "notice");
  assert.equal(seen[1].event, "progress");
});

test("imports.attach throws the terminal error", async () => {
  const chunks = ['event: error\ndata: {"status":409,"message":"taken"}\n\n'];
  const { fetch } = fakeFetch(() => sseResponse(chunks));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await assert.rejects(client.imports.attach("i-1", () => {}), (err) => {
    assert.equal(err.status, 409);
    assert.match(err.message, /taken/);
    return true;
  });
});

test("imports.attach sends Accept: text/event-stream", async () => {
  const { fetch, calls } = fakeFetch(() =>
    sseResponse(['event: result\ndata: {"repo":"acme/r"}\n\n'])
  );
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.imports.attach("i-1", () => {});
  assert.equal(calls[0].init.headers["Accept"], "text/event-stream");
});

test("imports.start forwards the dangerous confirm flag (dialog checkbox)", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ task: { id: "i-1" }, target: "acme/r" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.imports.start({ source_url: "https://git.example.com/a/b.git", owner: "acme", name: "r", dangerous: true });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.dangerous, true);
});

test("normalizeSource: shorthand, GitHub URL, generic canonical hint, ssh warning", () => {
  assert.deepEqual(normalizeSource("acme/monorepo"), {
    url: "https://github.com/acme/monorepo.git",
    kind: "github",
    owner: "acme",
    name: "monorepo",
  });
  assert.deepEqual(normalizeSource("https://github.com/acme/monorepo"), {
    url: "https://github.com/acme/monorepo.git",
    kind: "github",
    owner: "acme",
    name: "monorepo",
  });
  assert.deepEqual(normalizeSource("https://git.example.com/a/b.git"), {
    url: "https://git.example.com/a/b",
    kind: "generic",
    owner: "a",
    name: "b",
  });
  const ssh = normalizeSource("git@github.com:acme/monorepo.git");
  assert.ok(ssh.error);
  assert.equal(normalizeSource("").error !== undefined, true);
});

// Forgejo #401: the generic hint mirrors the server's canonicalGenericURL
// (internal/repoimport/url.go:196-208) — the EXACT variant rows from
// issue237_test.go:27-35 plus the bare canonical form must all collapse
// to the single canonical string the server gates and clones.
test("normalizeSource: generic variants collapse to the server canonical form (#401)", () => {
  const canon = "https://git.packden.us/crueber/dotfiles";
  const variants = [
    "https://git.packden.us/crueber/dotfiles.git",
    "https://git.packden.us:443/crueber/dotfiles.git",
    "https://git.packden.us/crueber/dotfiles/",
    "https://git.packden.us/crueber/dotfiles.git/",
    "https://GIT.PACKDEN.US/crueber/dotfiles.git",
    "https://git.packden.us/crueber/dotfiles",
  ];
  for (const v of variants) {
    const got = normalizeSource(v);
    assert.equal(got.url, canon, `normalizeSource(${v})`);
    assert.equal(got.kind, "generic", `kind(${v})`);
    assert.equal(got.owner, "crueber", `owner(${v})`);
    assert.equal(got.name, "dotfiles", `name(${v})`);
  }
  // http twin stays a distinct canonical source (scheme preserved), with
  // its default :80 folded — same as the server.
  assert.equal(
    normalizeSource("http://git.packden.us/crueber/dotfiles.git").url,
    "http://git.packden.us/crueber/dotfiles"
  );
  assert.equal(
    normalizeSource("http://git.packden.us:80/crueber/dotfiles.git").url,
    "http://git.packden.us/crueber/dotfiles"
  );
  // GitHub-path behavior is unchanged (canonical .git form).
  assert.equal(
    normalizeSource("https://github.com/acme/monorepo").url,
    "https://github.com/acme/monorepo.git"
  );
  // Anything the server would refuse stays verbatim for the server to
  // 400 (non-default port, embedded credentials) — the hint never
  // pretends a refused string is canonical.
  assert.equal(
    normalizeSource("https://git.packden.us:8443/crueber/dotfiles.git").url,
    "https://git.packden.us:8443/crueber/dotfiles.git"
  );
  assert.equal(
    normalizeSource("https://user:token@git.packden.us/crueber/dotfiles.git").url,
    "https://user:token@git.packden.us/crueber/dotfiles.git"
  );
});
