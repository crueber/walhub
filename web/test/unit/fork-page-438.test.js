// web/test/unit/fork-page-438.test.js — Forgejo #438 regression: the fork
// form is a standalone top-level page (site header only, no repo tab
// strip/sidebar), with cleaned-up copy, single-column fields at phone
// widths, and a Fork/Clone action pair in the repo header (text-only per
// #438, led by the shared #465 icons since #465).
// No DOM: routes/JSX/copy pinned as source text, mirroring
// repo-tabs-narrow.test.js. The SDK is untouched by #438 (fork.test.js
// pins the wire shapes); summary.forks still feeds the pill count.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const INDEX = srcOf("../../src/index.jsx");
const FORK = srcOf("../../src/pages/Fork.jsx");
const REPO = srcOf("../../src/pages/Repo.jsx");
const AGENTS = srcOf("../../../AGENTS.md");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("fork renders as a standalone top-level page, not inside the repo shell", () => {
  // The route lives outside the Repo parent (same URL as before, so the
  // repo-header Fork pill and every existing link keep working — no dead
  // link, no redirect needed).
  assert.ok(
    INDEX.includes('<Route path="/:owner/:name/fork" component={Fork} />'),
    "fork must be a top-level /:owner/:name/fork route",
  );
  const nested = block(INDEX, '<Route path="/:owner/:name" component={Repo}>', "<Route path=");
  assert.ok(!nested.includes("component={Fork}"), "fork must not be nested inside the Repo route");
  // Page-level framing (the PullNew precedent): centered content column
  // under the site header, never the repo tab strip.
  const shell = block(FORK, '<div class="fork-page', ">");
  assert.ok(shell.includes("mx-auto"), "fork page centers itself like other top-level composers");
  assert.ok(shell.includes("max-w-2xl"), "fork page keeps its narrow composer width");
  // Fork.jsx needs no repo context: owner/name come from route params, so
  // leaving the nested layout loses nothing.
  assert.ok(FORK.includes("useParams()"), "fork reads owner/name from route params (standalone-safe)");
  assert.ok(!FORK.includes("useRepo"), "fork must not depend on the Repo context it no longer renders inside");
});

test("copy cleanup: hints gone, Default branch everywhere", () => {
  assert.ok(!FORK.includes("you and your orgs only"), "owner hint removed");
  assert.ok(!FORK.includes("becomes the fork's default branch"), "branch hint removed");
  assert.ok(!FORK.includes("Starting branch"), '"Starting branch" is gone (visible label + aria-labels)');
  assert.ok(
    FORK.includes(">Default branch</span>"),
    '"Default branch" is the visible label',
  );
  const ariaCount = (FORK.match(/aria-label="Default branch"/g) ?? []).length;
  assert.equal(ariaCount, 2, "both branch selects (loading + loaded) carry the Default branch aria-label");
  assert.ok(FORK.includes("<option value=\"\">parent default</option>"), "parent-default option stays");
});

test("form fields stack single-column at phone widths", () => {
  const grids = FORK.match(/<div class="grid [^"]*">/g) ?? [];
  const fieldRows = grids.filter((g) => g.includes("grid-cols-2"));
  assert.equal(fieldRows.length, 2, "both field rows (Owner/Name, Visibility/Default branch) stay paired on desktop");
  for (const g of fieldRows) {
    assert.ok(
      g.includes("grid-cols-1") && g.includes("sm:grid-cols-2"),
      `field row must collapse to one column below sm: ${g}`,
    );
  }
  assert.ok(!FORK.includes("grid-cols-2 gap-3"), "no unprefixed two-column row may force side-by-side fields at 390px");
});

test("390px arithmetic: stacked fields fit the viewport", () => {
  // Page padding px-4 = 32px, so the form cell is 390 − 32 = 358px wide.
  // Single-column rows are exactly one field wide, and every field control
  // is a fluid input/select (no fixed-width element), so the widest row is
  // the 358px cell itself — the page contributes zero overflow
  // (scrollWidth === clientWidth).
  const viewport = 390;
  const formClient = viewport - 32;
  assert.equal(formClient, 358, "form client width at a 390px viewport");
  assert.ok(FORK.includes('class="card grid gap-3 p-4"'), "form keeps its fluid card shape (no fixed width)");
});

test("repo header Fork and Clone carry the shared icons on the canonical btn idiom", () => {
  // Forgejo #447 builds on the #438 text-only pair: Fork and the CloneMenu
  // summary now share the Star/Watch btn metrics (btn px-2 py-1 text-sm) at
  // the same size/weight — one action strip, not two families. Forgejo #465
  // supersedes the text-only half: both lead with the shared SVG Icon (the
  // retired ⑂ mark never comes back). The count still reads summary.forks
  // (no extra fetch); the count-left-of-label shape itself is pinned in
  // header-pills-447.test.js.
  const pill = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "<CloneMenu");
  assert.ok(pill.includes('<span class="btn px-2 py-1 text-sm'), "Fork keeps the canonical btn metrics on one pill shell (was pill pre-#447, split links since #464)");
  // Forgejo #502: the label href goes through forkHref() — the fork page
  // for writers, the log-in interstitial (next = fork page) for anonymous
  // viewers. The fork-page destination itself is pinned below.
  assert.ok(pill.includes("href={forkHref()}"), "Fork label routes through the #502 write gate");
  assert.ok(REPO.includes("`/${full()}/fork`"), "the gate preserves the fork-page destination for writers");
  assert.ok(pill.includes('href={`/${full()}/forks`}'), "Fork count links to the fork-network page (#464 split)");
  assert.ok(pill.includes("summary.forks") || pill.includes("s().forks"), "Fork count still reads summary.forks (no extra fetch)");
  assert.ok(pill.includes('<Icon name="fork"'), "Fork pill leads with the shared fork icon (#465)");
  assert.ok(!pill.includes("⑂"), "the retired glyph stays gone");
  const cloneSummary = block(REPO, "<summary class=\"btn", "</summary>");
  assert.ok(cloneSummary.includes("px-2 py-1 text-sm"), "Clone summary keeps the canonical btn metrics (the matched pair)");
  assert.ok(cloneSummary.includes('<Icon name="clone"'), "Clone summary leads with the shared clone icon (#465)");
  assert.ok(cloneSummary.endsWith('<Icon name="clone" /> Clone'), "Clone summary renders icon left of the Clone label, no count");
});

test("AGENTS.md carries the normative mobile-viewport rule", () => {
  assert.ok(AGENTS.includes("Mobile viewport is always checked."), "the §2 rule exists with normative wording");
  assert.ok(AGENTS.includes("~390px"), "the rule names the narrow-viewport width");
  assert.ok(
    AGENTS.includes("UI work without a mobile-viewport check is not done."),
    "the rule is stated in the same normative register as the other working rules",
  );
});
