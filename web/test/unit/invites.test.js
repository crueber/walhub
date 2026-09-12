// web/test/unit/invites.test.js — invite display rules (Forgejo #362):
// expiry predicate, kind/scope labels, and the API-accept-URL deep-link
// parser. Pure lib — no DOM.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  isInviteExpired,
  inviteKind,
  inviteScope,
  invitePageLink,
} from "../../src/lib/invites.js";

const NOW = new Date("2026-09-12T12:00:00Z").getTime();

test("isInviteExpired reads expires_at against now", () => {
  assert.equal(isInviteExpired({ expires_at: "2026-09-12T11:59:59Z" }, NOW), true);
  assert.equal(isInviteExpired({ expires_at: "2026-09-12T12:00:00Z" }, NOW), true); // boundary: at == expired
  assert.equal(isInviteExpired({ expires_at: "2026-09-12T12:00:01Z" }, NOW), false);
  assert.equal(isInviteExpired({ expires_at: "2026-09-19T12:00:00Z" }, NOW), false); // 7-day TTL still pending
});

test("isInviteExpired fails open on missing/unparseable expiry", () => {
  for (const inv of [null, undefined, {}, { expires_at: "" }, { expires_at: null }, { expires_at: "not-a-date" }]) {
    assert.equal(isInviteExpired(inv, NOW), false, `must not read expired: ${JSON.stringify(inv)}`);
  }
});

test("inviteKind prefers kind, derives from scope for inbox rows", () => {
  assert.equal(inviteKind({ kind: "org", org: "acme" }), "org");
  assert.equal(inviteKind({ kind: "repo", repo: "acme/r" }), "repo");
  assert.equal(inviteKind({ org: "acme" }), "org"); // mine() row
  assert.equal(inviteKind({ repo: "acme/r" }), "repo"); // mine() row
  assert.equal(inviteKind({}), "org");
  assert.equal(inviteKind(null), "org");
});

test("inviteScope names the target", () => {
  assert.equal(inviteScope({ kind: "repo", org: "acme", repo: "acme/r" }), "acme/r");
  assert.equal(inviteScope({ kind: "org", org: "acme" }), "acme");
  assert.equal(inviteScope({}), "");
  assert.equal(inviteScope(null), "");
});

test("invitePageLink converts the API accept URL to an inbox deep link", () => {
  assert.equal(
    invitePageLink("/api/v1/invitations/abc123?token=tok456"),
    "/invitations?id=abc123&token=tok456"
  );
  assert.equal(invitePageLink(""), "");
  assert.equal(invitePageLink(null), "");
  assert.equal(invitePageLink("/invitations?id=x&token=y"), ""); // already a page link: no double-wrap
  assert.equal(invitePageLink("/api/v1/invitations/abc123"), ""); // no token: nothing to carry
});
