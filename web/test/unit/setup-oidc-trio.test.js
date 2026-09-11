// web/test/unit/setup-oidc-trio.test.js — Forgejo #344: /setup warns when OIDC
// is selected without the browser-login trio (mirrors config.Validate, which
// refuses to boot). Each missing key fails on its own row, naming it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { validateSetup } from "../../src/lib/setup.js";

const oidcBase = () => ({
  "server.listen": "0.0.0.0:8080",
  "server.auth.mode": "oidc",
  "server.auth.anonymous_read": "false",
  "server.auth.allowed_domains": "example.com",
  "server.auth.issuer": "https://id.example.com",
  "server.auth.session_secret": "0123456789abcdef0123456789abcdef",
  "server.auth.oauth_client_id": "walhub-web",
  "server.auth.oauth_client_secret": "s3cret",
  "store.backend": "filesystem",
  "store.root": "/var/lib/walhub/store",
  "cache.dir": "/var/cache/walhub",
});

const fatals = (values) => validateSetup(values).filter((e) => e.severity === "error");

test("complete OIDC config passes with no errors", () => {
  assert.deepEqual(fatals(oidcBase()), []);
});

const TRIO_KEYS = [
  "server.auth.session_secret",
  "server.auth.oauth_client_id",
  "server.auth.oauth_client_secret",
];
for (const key of TRIO_KEYS) {
  test(`oidc without ${key} fails naming the missing key`, () => {
    const values = oidcBase();
    values[key] = "";
    const errs = fatals(values).filter((e) => e.key === key);
    assert.ok(errs.length > 0, `expected an error on ${key}, got ${JSON.stringify(fatals(values))}`);
    assert.match(errs[0].message, new RegExp(key.replace(/\./g, "\\.")));
  });
}

test("oidc without the whole trio names all three keys", () => {
  const values = oidcBase();
  for (const key of TRIO_KEYS) values[key] = "";
  const errs = fatals(values);
  for (const key of TRIO_KEYS) {
    assert.ok(errs.some((e) => e.key === key && e.message.includes(key)), `expected ${key} named, got ${JSON.stringify(errs)}`);
  }
});

test("non-oidc modes do not require the trio", () => {
  for (const mode of ["none", "token"]) {
    const values = oidcBase();
    values["server.auth.mode"] = mode;
    for (const key of TRIO_KEYS) values[key] = "";
    const errs = fatals(values).filter((e) => TRIO_KEYS.includes(e.key) && /trio|required in oidc mode/.test(e.message));
    assert.deepEqual(errs, [], `mode ${mode} must not require the trio`);
  }
});
