// web/test/unit/clone.test.js — clone-dialog URL builders (issue #37):
// the HTTPS URL keeps the server's clone_url verbatim, the SSH URL reuses
// its host at the default ssh port (never the HTTP port), and copyText
// prefers the Clipboard API with an execCommand fallback.

import { test, afterEach } from "node:test";
import assert from "node:assert/strict";

import { httpsCloneUrl, sshCloneUrl, cloneCommand, copyText } from "../../src/lib/clone.js";

test("httpsCloneUrl prefers the server clone_url, falls back to origin", () => {
  assert.equal(
    httpsCloneUrl({ clone_url: "https://h/o/r.git" }, "o/r", "https://h"),
    "https://h/o/r.git",
  );
  assert.equal(httpsCloneUrl(null, "o/r", "https://h"), "https://h/o/r.git");
  assert.equal(httpsCloneUrl({}, "o/r", "https://h"), "https://h/o/r.git");
});

test("sshCloneUrl reuses the https host, drops scheme and http port", () => {
  // The BEFORE case from the issue body: a LAN http URL with an http port.
  assert.equal(
    sshCloneUrl("http://192.168.2.48:8080/crueber/walhub.git", "crueber/walhub", "x"),
    "ssh://git@192.168.2.48/crueber/walhub.git",
  );
  assert.equal(
    sshCloneUrl("https://git.packden.us/crueber/walhub.git", "crueber/walhub", "x"),
    "ssh://git@git.packden.us/crueber/walhub.git",
  );
  assert.equal(
    sshCloneUrl("https://h:8443/o/r.git", "o/r", "x"),
    "ssh://git@h/o/r.git",
  );
});

test("sshCloneUrl falls back to the page host, then localhost", () => {
  assert.equal(sshCloneUrl("not a url", "o/r", "page.host"), "ssh://git@page.host/o/r.git");
  assert.equal(sshCloneUrl("not a url", "o/r"), "ssh://git@localhost/o/r.git");
});

test("cloneCommand prefixes git clone", () => {
  assert.equal(cloneCommand("https://h/o/r.git"), "git clone https://h/o/r.git");
});

afterEach(() => {
  delete globalThis.navigator;
  delete globalThis.document;
});

test("copyText uses the Clipboard API when available", async () => {
  let got = null;
  globalThis.navigator = { clipboard: { writeText: async (t) => { got = t; } } };
  assert.equal(await copyText("git clone https://h/o/r.git"), true);
  assert.equal(got, "git clone https://h/o/r.git");
});

test("copyText falls back to execCommand when clipboard throws", async () => {
  globalThis.navigator = { clipboard: { writeText: async () => { throw new Error("denied"); } } };
  globalThis.document = { execCommand: () => true };
  assert.equal(await copyText("x"), true);
});

test("copyText falls back to execCommand with no clipboard at all", async () => {
  globalThis.document = { execCommand: () => true };
  assert.equal(await copyText("x"), true);
});

test("copyText reports false when nothing can copy", async () => {
  assert.equal(await copyText("x"), false);
  globalThis.document = { execCommand: () => false };
  assert.equal(await copyText("x"), false);
});
