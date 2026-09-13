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
  // Forgejo #497: the row lives in the shared OwnerNameRow component —
  // both pages render it, so the layout is identical by construction. The
  // component keeps the collapsing rule (grid-cols-1 below sm:, never a
  // bare grid-cols-2) with an asymmetric sm: split (Owner 1fr, Name 2fr).
  const ROW = srcOf("../../src/components/OwnerNameRow.jsx");
  for (const [name, src] of [["New.jsx", NEW], ["Import.jsx", IMPORT]]) {
    assert.ok(
      src.includes("<OwnerNameRow"),
      `${name}: Owner/Name row is the shared component`,
    );
    assert.ok(
      !/<div class="[^"]*grid-cols-2[^"]*">/.test(src),
      `${name}: no in-page two-column row may squeeze fields at 390px`,
    );
  }
  assert.ok(
    ROW.includes('<div class="grid grid-cols-1 items-start gap-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">'),
    "shared row: collapsing base with asymmetric sm: split",
  );
});

test("owner hint noise is gone from both owner selects", () => {
  assert.ok(!NEW.includes("you and your orgs only"), "New.jsx: hint dropped");
  assert.ok(!IMPORT.includes("you and your orgs only"), "Import.jsx: hint dropped");
});

test("mirror pull-only copy exists once, on Import.jsx (Forgejo #487)", () => {
  // Forgejo #487 removed the /new mirror mode: New.jsx carries no mirror
  // UI at all, and the paragraph moved to Import.jsx — the sole mirror
  // path. Import's mirror radio label still reads "(recurring pull, pushes
  // rejected)", and the paragraph below the schedule select carries the
  // full model (pull-only, pushes rejected, first sync immediate).
  const rejected = (src) => (src.match(/pushes are rejected/g) ?? []).length;
  assert.equal(rejected(NEW) + rejected(IMPORT), 1, "the pull-only paragraph renders exactly once total");
  assert.ok(!NEW.includes("pull-only, scheduled syncs"), "New mirror radio label gone with the mode");
  assert.ok(!NEW.includes("new-source") && !NEW.includes("new-schedule"), "New carries no mirror fields");
  assert.ok(IMPORT.includes("(recurring pull, pushes rejected)"), "Import mirror radio label unchanged");
  assert.ok(IMPORT.includes("pushes are rejected for everyone"), "paragraph carries the push-rejection model");
  assert.ok(IMPORT.includes("The first sync starts immediately"), "paragraph carries the first-sync model");
  // The kept paragraph is field help, wired to the schedule it explains.
  assert.ok(IMPORT.includes('id="import-mirror-help"'), "kept paragraph carries an id");
  assert.ok(IMPORT.includes('aria-describedby="import-mirror-help"'), "schedule select references the paragraph");
  assert.ok(!NEW.includes("new-mirror-help"), "New keeps no mirror help wiring");
});

test("Import LFS/ssh limits collapse into a details, not prose", () => {
  assert.ok(IMPORT.includes("<details"), "limits live under a <details>");
  assert.ok(IMPORT.includes("never smudged"), "LFS pointer-blob note kept");
  assert.ok(IMPORT.includes("Server-side ssh is not"), "ssh limitation note kept");
  const prose = (IMPORT.match(/<p class="muted text-xs">\n\s*LFS-tracked/g) ?? []).length;
  assert.equal(prose, 0, "no always-rendered LFS/ssh prose paragraph remains");
});

test("help text is field-scoped with id + aria-describedby", () => {
  // Forgejo #486 supersedes the always-on repo-name helpers: charset
  // guidance on New/Import lives in the live-validation message only
  // (see repo-name-486.test.js) — the name-help pairs are gone by design.
  const pairs = [
    ["Import.jsx", IMPORT, "import-mirror-help"],
    ["Import.jsx", IMPORT, "import-source-help"],
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
  assert.ok(NEW.includes("repos.repos.create("), "New still creates via repos.repos.create");
  assert.ok(!NEW.includes("repos.mirrors.create("), "New mirror mode is gone (Forgejo #487)");
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
