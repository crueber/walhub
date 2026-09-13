// web/test/unit/create-forms-479.test.js — Forgejo #479: the three
// standalone create forms (New repository / Import repository / New
// organization) follow the canonical form-page pattern (reference:
// ReleaseNew.jsx; grid idiom: Fork.jsx). Visual/structure only — submit
// targets, validation, errors, and navigation are unchanged.
// No DOM: routes/JSX/copy pinned as source text, mirroring
// fork-page-438.test.js.
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
const ORGNEW = srcOf("../../src/pages/OrgNew.jsx");

test("Owner/Name rows collapse below sm (no bare grid-cols-2)", () => {
  for (const [name, src] of [["New.jsx", NEW], ["Import.jsx", IMPORT]]) {
    assert.ok(
      src.includes('<div class="grid grid-cols-1 gap-3 sm:grid-cols-2">'),
      `${name}: Owner/Name row uses the canonical collapsing row class`,
    );
    assert.ok(
      !src.includes("grid-cols-2 gap-3"),
      `${name}: no unprefixed two-column row may squeeze fields at 390px`,
    );
  }
});

test("owner hint noise is gone from both owner selects", () => {
  assert.ok(!NEW.includes("you and your orgs only"), "New.jsx: hint dropped");
  assert.ok(!IMPORT.includes("you and your orgs only"), "Import.jsx: hint dropped");
});

test("mirror pull-only copy exists once, on New.jsx", () => {
  // The paragraph carries the schedule/first-sync detail where the mirror
  // mode is the secondary option; Import's mirror radio label already says
  // "(recurring pull, pushes rejected)", so the paragraph adds nothing there.
  const rejected = (src) => (src.match(/pushes are rejected/g) ?? []).length;
  assert.equal(rejected(NEW) + rejected(IMPORT), 1, "the pull-only paragraph renders exactly once total");
  assert.ok(NEW.includes("(pull-only, scheduled syncs)"), "New mirror radio label unchanged");
  assert.ok(IMPORT.includes("(recurring pull, pushes rejected)"), "Import mirror radio label unchanged");
  // The kept paragraph is field help, wired to the schedule it explains.
  assert.ok(NEW.includes('id="new-mirror-help"'), "kept paragraph carries an id");
  assert.ok(NEW.includes('aria-describedby="new-mirror-help"'), "schedule select references the paragraph");
});

test("Import LFS/ssh limits collapse into a details, not prose", () => {
  assert.ok(IMPORT.includes("<details"), "limits live under a <details>");
  assert.ok(IMPORT.includes("never smudged"), "LFS pointer-blob note kept");
  assert.ok(IMPORT.includes("Server-side ssh is not"), "ssh limitation note kept");
  const prose = (IMPORT.match(/<p class="muted text-xs">\n\s*LFS-tracked/g) ?? []).length;
  assert.equal(prose, 0, "no always-rendered LFS/ssh prose paragraph remains");
});

test("help text is field-scoped with id + aria-describedby", () => {
  const pairs = [
    ["New.jsx", NEW, "new-name-help"],
    ["New.jsx", NEW, "new-mirror-help"],
    ["Import.jsx", IMPORT, "import-source-help"],
    ["Import.jsx", IMPORT, "import-name-help"],
    ["Import.jsx", IMPORT, "import-token-help"],
    ["OrgNew.jsx", ORGNEW, "orgnew-name-help"],
  ];
  for (const [name, src, id] of pairs) {
    assert.ok(src.includes(`id="${id}"`), `${name}: help element #${id} exists`);
    assert.ok(src.includes(`aria-describedby="${id}"`), `${name}: a control references #${id}`);
  }
  // Help copy mirrors the enforced rules (never invents its own).
  assert.ok(
    ORGNEW.includes("Lowercase letters, digits, and hyphens, 1–39 characters"),
    "org name help matches the validateOrgName rule text",
  );
  assert.ok(
    IMPORT.includes("never stored or logged"),
    "token help keeps the never-stored promise",
  );
});

test("cancel treatment is identical across all three forms", () => {
  // Implementer's call (issue leaves the direction open): every form gets
  // OrgNew's secondary cancel to the natural back surface (/explore).
  for (const [name, src] of [["New.jsx", NEW], ["Import.jsx", IMPORT], ["OrgNew.jsx", ORGNEW]]) {
    assert.ok(src.includes('<A class="btn px-3 py-1" href="/explore">'), `${name}: cancel affordance present`);
    assert.ok(src.includes("cancel\n"), `${name}: cancel label present`);
  }
});

test("layout shell matches the canonical centered column", () => {
  for (const [name, src] of [["New.jsx", NEW], ["Import.jsx", IMPORT], ["OrgNew.jsx", ORGNEW]]) {
    assert.ok(src.includes("mx-auto"), `${name}: centered column`);
    assert.ok(src.includes("max-w-2xl"), `${name}: narrow composer width`);
    assert.ok(src.includes('<form class="card grid gap-3 p-4"'), `${name}: single card form shell`);
  }
});

test("no behavior change: submit targets, validation, navigation", () => {
  assert.ok(NEW.includes("repos.repos.create("), "New empty mode still creates via repos.repos.create");
  assert.ok(NEW.includes("repos.mirrors.create("), "New mirror mode still creates via repos.mirrors.create");
  assert.ok(NEW.includes("navigate(`/${full}`)"), "New still navigates to the created repo");
  assert.ok(IMPORT.includes("repos.imports.start("), "Import still starts via repos.imports.start");
  assert.ok(IMPORT.includes("repos.mirrors.create(payload"), "Import mirror mode still creates via repos.mirrors.create");
  assert.ok(ORGNEW.includes("repos.orgs.create("), "OrgNew still creates via repos.orgs.create");
  assert.ok(ORGNEW.includes("navigate(`/${slug}/settings`)"), "OrgNew still lands in the org settings");
  assert.ok(NEW.includes("validateRepoName(getOwner(), getName())"), "New validation unchanged");
  assert.ok(ORGNEW.includes("validateOrgName(getOrg())"), "OrgNew validation unchanged");
});

test("standing rule recorded in the file headers", () => {
  for (const [name, src] of [["New.jsx", NEW], ["Import.jsx", IMPORT], ["OrgNew.jsx", ORGNEW]]) {
    assert.ok(src.includes("Forgejo #479"), `${name}: header cites the issue`);
    assert.ok(src.includes("ReleaseNew.jsx"), `${name}: header references the canonical implementation`);
  }
});
