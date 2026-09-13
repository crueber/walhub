// web/test/unit/new-reserve-pushonly-487.test.js — Forgejo #487: the New
// repository page is reserve/push-only (no mirror mode); Import is the sole
// mirror path. No DOM: routes/JSX/copy pinned as source text, mirroring
// fork-page-438.test.js and create-forms-479.test.js. The shared mirror
// lib (web/src/lib/mirror.js) is untouched — mirror.test.js owns it; here
// the Import mirror submit path is exercised once through the shared
// validateMirrorCreate rule to confirm the surviving UI path still creates
// mirrors.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const NEW = srcOf("../../src/pages/New.jsx");
const IMPORT = srcOf("../../src/pages/Import.jsx");

test("New has no mirror UI: no kind radiogroup, no source URL, no schedule", () => {
  assert.ok(!NEW.includes('aria-label="Repository kind"'), "kind radiogroup gone (one kind only)");
  assert.ok(!NEW.includes("mirror from URL"), "mirror radio gone");
  assert.ok(!NEW.includes("mirror continuously"), "no mirror radio of any spelling");
  assert.ok(!NEW.includes('id="new-source"'), "no source-URL field");
  assert.ok(!NEW.includes('aria-label="Source URL"'), "no source-URL field by aria label");
  assert.ok(!NEW.includes('id="new-schedule"'), "no schedule select");
  assert.ok(!NEW.includes('aria-label="Sync schedule"'), "no schedule select by aria label");
  assert.ok(!NEW.includes("create mirror"), "no mirror button label");
});

test("New carries no dead mirror signals, branches, or imports", () => {
  for (const dead of ["getMode", "setMode", "getSource", "setSource", "getSchedule", "setSchedule", "mirrorError"]) {
    assert.ok(!NEW.includes(dead), `no dead signal/branch: ${dead}`);
  }
  assert.ok(!NEW.includes("MIRROR_PRESETS"), "MIRROR_PRESETS import removed");
  assert.ok(!NEW.includes("validateMirrorCreate"), "validateMirrorCreate import removed");
  assert.ok(!NEW.includes("DEFAULT_MIRROR_PRESET"), "DEFAULT_MIRROR_PRESET import removed");
  assert.ok(!NEW.includes("from \"../lib/mirror.js\""), "mirror lib import removed");
  assert.ok(!NEW.includes("repos.mirrors.create("), "no mirror submit path");
});

test("New top copy states the push-first model and points mirrors at Import", () => {
  assert.ok(NEW.includes("Reserve a name and get push instructions"), "push-first intro kept");
  assert.ok(NEW.includes("nothing to approve, never a conflict"), "placeholder model (no approval, no conflict) kept");
  assert.ok(NEW.includes('href="/import"'), "mirror-seekers are pointed at Import");
  assert.ok(NEW.includes("create repository"), "single create label (no swap)");
});

test("Import mirror mode intact: sole UI path to a mirror", () => {
  assert.ok(IMPORT.includes('checked={getMode() === "mirror"}'), "mirror radio kept");
  assert.ok(IMPORT.includes("mirror continuously"), "mirror radio label kept");
  assert.ok(IMPORT.includes('aria-label="Sync schedule"'), "schedule select kept");
  assert.ok(IMPORT.includes("MIRROR_PRESETS"), "schedule presets still shared");
  assert.ok(IMPORT.includes("validateMirrorCreate"), "shared validation still wired");
  assert.ok(IMPORT.includes("repos.mirrors.create(payload"), "mirror submit still targets the create-from-URL endpoint");
  assert.ok(IMPORT.includes("navigate(`/${target}`)"), "mirror create still lands on the repo page");
});

test("Import mirror help copy renders the full pull-only model", () => {
  assert.ok(IMPORT.includes("pushes are rejected for everyone"), "push-rejection model kept");
  assert.ok(IMPORT.includes("syncs on the schedule"), "schedule model kept");
  assert.ok(IMPORT.includes("The first sync starts immediately"), "first-sync model kept");
  assert.ok(IMPORT.includes('id="import-mirror-help"'), "help paragraph carries an id");
});

test("Import mirror submit path exercised once through the shared rule", async () => {
  // The surviving UI path validates via validateMirrorCreate before POSTing
  // to /api/v1/repos/mirrors (sdk-mirror.test.js pins the endpoint/method).
  // Exercise the rule exactly as Import.jsx calls it: bad input rejected,
  // good input passes to the create call.
  const { validateMirrorCreate, DEFAULT_MIRROR_PRESET } = await import("../../src/lib/mirror.js");
  const bad = validateMirrorCreate({ sourceUrl: "", owner: "acme", name: "x", schedule: DEFAULT_MIRROR_PRESET });
  assert.ok(bad.error, "empty source rejected before any POST");
  const good = validateMirrorCreate({
    sourceUrl: "https://github.com/acme/upstream.git",
    owner: "acme",
    name: "x",
    schedule: DEFAULT_MIRROR_PRESET,
  });
  assert.deepEqual(good, {}, "valid mirror input passes to repos.mirrors.create");
  assert.ok(
    IMPORT.includes("source_url: getUrl().trim()") && IMPORT.includes("schedule: getSchedule()"),
    "Import shapes the POST body from the same fields the rule validated",
  );
});
