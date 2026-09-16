import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

/** Forgejo #601: user-avatar upload — SDK method + owner-page control pins. */

const BASE = "http://api.test";

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPOS = srcOf("../../src/pages/Repos.jsx");
const USERS_SDK = srcOf("../../sdk/src/users.js");

test("users.avatar.upload hits PUT on the avatar path (org-twin verb)", async () => {
  const calls = [];
  const fetch = async (url, init = {}) => {
    calls.push({ url: String(url).replace(BASE, ""), method: init.method, body: init.body });
    return jsonResponse({ ok: true });
  };
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.users.avatar.upload("Alice", new Uint8Array([1, 2, 3]));
  assert.equal(calls[0].method, "PUT");
  assert.equal(calls[0].url, "/api/v1/users/alice/avatar");
  assert.ok(calls[0].body instanceof Uint8Array, "raw bytes ride the body (no JSON envelope)");
});

test("users.avatar.upload accepts a Blob/File (form file input shape)", async () => {
  if (typeof Blob === "undefined" || typeof File === "undefined") return;
  const calls = [];
  const fetch = async (url, init = {}) => {
    calls.push({ url: String(url).replace(BASE, ""), method: init.method });
    return jsonResponse({ ok: true });
  };
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.users.avatar.upload("alice", new File([new Uint8Array([9, 9])], "a.png", { type: "image/png" }));
  assert.equal(calls[0].method, "PUT");
  assert.equal(calls[0].url, "/api/v1/users/alice/avatar");
});

test("Repos #601: upload control rides the self-only action stack", () => {
  // The file input lives inside the isSelf() Show, next to Regenerate/
  // Remove — the server re-checks self-or-admin; the client never decides.
  const selfBlock = REPOS.slice(REPOS.indexOf("<Show when={isSelf()}>"));
  assert.ok(selfBlock.includes('type="file"'), "self block carries the upload file input");
  assert.ok(selfBlock.includes("uploadAvatar"), "self block wires the upload handler");
  assert.ok(selfBlock.includes("Regenerate avatar"), "Regenerate stays beside the upload (POST replaces it)");
  assert.ok(selfBlock.includes("Remove avatar"), "Remove stays beside the upload");
});

test("Repos #601: upload input accepts PNG/JPEG/GIF only (WebP 415s server-side)", () => {
  assert.ok(
    REPOS.includes('accept="image/png,image/jpeg,image/gif"'),
    "file picker offers exactly the server allowlist — no WebP, no SVG"
  );
  assert.ok(
    REPOS.includes("MAX_USER_AVATAR_BYTES = 2 << 20"),
    "2 MiB client pre-check mirrors the server cap"
  );
  assert.ok(
    REPOS.includes("only PNG, JPEG, and GIF avatars are accepted (WebP is not supported)"),
    "415 maps to the allowlist note (names the WebP divergence)"
  );
  assert.ok(REPOS.includes("avatar too large (max 2 MiB)"), "413 maps to the size note");
});

test("Repos #601: upload reuses the circular preview + the shared refresh", () => {
  assert.ok(REPOS.includes("h-24 w-24 rounded-full"), "preview stays the 96px circle (upload renders into it)");
  const fn = REPOS.slice(REPOS.indexOf("const uploadAvatar"), REPOS.indexOf("const regenerateAvatar"));
  assert.ok(fn.includes("refreshAvatar()"), "upload refreshes through the shared avatar refresh");
  const refresh = REPOS.slice(REPOS.indexOf("const refreshAvatar"), REPOS.indexOf("const uploadAvatar"));
  assert.ok(refresh.includes('invalidate(`user:${owner()}`)'), "refresh busts the user cache key (?v= refreshes)");
  assert.ok(refresh.includes('invalidate("me")'), "refresh busts the shared me key (navbar refreshes)");
});

test("Repos #601: circular preview survives (IdentityMenu/Repos display contract)", () => {
  assert.ok(REPOS.includes("rounded-full"), "display stays circular");
  assert.ok(REPOS.includes("ring-zinc-300"), "ring treatment unchanged");
});

test("users SDK #601: upload/regenerate/remove share the avatar path", () => {
  for (const verb of ['method: "PUT"', 'method: "POST"', 'method: "DELETE"']) {
    assert.ok(USERS_SDK.includes(verb), `SDK avatar group covers ${verb}`);
  }
  assert.ok(USERS_SDK.includes("avatar"), "avatar group exists on users");
});

test("Repos #601: no one-off CSS, no layout breakage at 390px", () => {
  // Tailwind-only (AGENTS.md): the control composes btn/muted/flex
  // utilities like its siblings — no <style> block, no new CSS file.
  // Forgejo #619 restyled the affordance (hidden input + .btn span +
  // muted helper below) — behavior (accept/onChange/gate) unchanged.
  const block = REPOS.slice(REPOS.indexOf("Upload profile image"), REPOS.indexOf("Regenerate avatar"));
  assert.ok(block.includes('class="'), "upload label composes utility classes");
  assert.ok(!block.includes("<style"), "no one-off CSS for the upload control");
  assert.ok(block.includes("w-full"), "input constrains to the sidebar column (no 390px overflow)");
});
