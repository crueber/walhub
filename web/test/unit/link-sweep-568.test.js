// web/test/unit/link-sweep-568.test.js — Forgejo #568: systemic .link
// sweep (follow-up to #566, deliberately unbundled).
//
// The `.link` class had ZERO CSS rules in shipped web/src/ui.css (the
// only bundled sheet); Tailwind v4 preflight resets those
// buttons/links to plain muted text. #566 fixed only the two composer
// Cancels (small secondary .btn); #567's review caught + fixed
// StagedCard (same .btn idiom). The remaining 25 uses across 10 files
// were all invisible text until this fix.
//
// Fix (option (a) from the issue — the #405 opaque-popover precedent):
// ONE shared `.link` component rule in ui.css @layer components
// (accent emerald both themes, hover underline, cursor-pointer), so all
// 25 sites — including future ones — resolve to a styled affordance
// with zero per-site composition. Button-shaped actions (composer
// Cancels, staged edit/remove) stay .btn by design, never .link
// (#566/#567).
//
// Pinned here: the shared rule exists with both-theme + hover + cursor
// tokens, every remaining class="link site keeps the class (the rule is
// the treatment — no per-site rewrite), the exact per-file census (25
// total, so a newly added or silently dropped site fails loudly), <A>
// behavior intact (router import + hrefs byte-identical, rule is a
// class — never a bare `a` rule fighting scoped link styles), the
// #566/#567 .btn surfaces untouched, no-new-deps, and the law-12 doc
// amendments (12_web_ui.md + style-guideline entry naming the
// canonical reference).

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const CSS = () => read("../../src/ui.css");
const DOC = () => read("../../../docs/go/12_web_ui.md");
const GUIDE = () => read("../../../docs/style-guideline.md");
const PAGE = (n) => read(`../../src/pages/${n}.jsx`);
const COMP = (n) => read(`../../src/components/${n}.jsx`);

const linkRule = () => {
  const css = CSS();
  const i = css.indexOf(".link {");
  assert.ok(i >= 0, "ui.css carries the shared .link rule");
  return css.slice(i, css.indexOf("}", i) + 1);
};

// --- 1. the shared rule: accent, both themes, hover + cursor ---

test("ui.css carries one shared .link rule in @layer components", () => {
  const css = CSS();
  assert.ok(css.includes("@layer components"), "@layer components still exists");
  const layer = css.slice(css.indexOf("@layer components"));
  assert.ok(layer.includes(".link {"), ".link rule lives in the components layer (shared pattern, not inline)");
  assert.equal((css.match(/\.link\s*\{/g) ?? []).length, 1, "exactly ONE .link rule — the shared treatment, no per-site duplicates");
});

test(".link rule carries the accent color in both themes", () => {
  const rule = linkRule();
  assert.ok(rule.includes("text-emerald-700"), "light theme: emerald accent (the markdown-link language)");
  assert.ok(rule.includes("dark:text-emerald-400"), "dark theme carries its own emerald (dark is the default theme)");
  assert.ok(rule.includes("dark:"), "both-theme tokens present — no bare unthemed color");
});

test(".link rule carries hover + cursor affordance", () => {
  const rule = linkRule();
  assert.ok(rule.includes("cursor-pointer"), "pointer cursor — buttons lose preflight's default, spans gain affordance");
  assert.ok(rule.includes("hover:underline"), "hover underline affordance");
  assert.ok(rule.includes("hover:text-emerald-600"), "light hover color shift");
  assert.ok(rule.includes("dark:hover:text-emerald-300"), "dark hover color shift");
});

test(".link rule justifies itself as a shared pattern (the #405 precedent)", () => {
  const css = CSS();
  const comment = css.slice(css.indexOf("Forgejo #568"), css.indexOf(".link {"));
  assert.ok(comment.includes("#405"), "comment cites the opaque-popover shared-rule precedent");
  assert.ok(comment.includes("25"), "comment states the site census it covers");
  assert.ok(/never a bare `a` rule|bare `a`/.test(comment), "comment records why a class, not a bare `a` rule");
});

// --- 2. census: all 25 sites keep class="link" (the rule is the treatment) ---

const EXPECTED = {
  "pages/Team": ["Team.jsx", 2],
  "pages/Settings": ["Settings.jsx", 4],
  "pages/PullFiles": ["PullFiles.jsx", 2],
  "pages/PullCommits": ["PullCommits.jsx", 1],
  "pages/Pull": ["Pull.jsx", 5],
  "pages/Invitations": ["Invitations.jsx", 1],
  "pages/Commit": ["Commit.jsx", 1],
  "pages/Checks": ["Checks.jsx", 5],
  "pages/CheckDetail": ["CheckDetail.jsx", 2],
  "components/NotificationTray": ["NotificationTray.jsx", 2],
};

test("per-file census: 25 class=\"link sites across 10 files", () => {
  let total = 0;
  for (const [key, [file, count]] of Object.entries(EXPECTED)) {
    const src = key.startsWith("pages/") ? PAGE(file.replace(".jsx", "")) : COMP(file.replace(".jsx", ""));
    const found = (src.match(/class="link/g) ?? []).length;
    assert.equal(found, count, `${file}: expected ${count} class="link sites, found ${found}`);
    total += found;
  }
  assert.equal(total, 25, "25 total sites — a newly added or silently dropped site fails here");
});

test("button action sites keep their handlers (behavior intact)", () => {
  const pull = PAGE("Pull");
  for (const needle of ["submitDismiss(rv.seq)", "remove(who)", "onClick={toggle}", "props.onUnstage(i())"]) {
    assert.ok(pull.includes(needle), `Pull.jsx behavior intact: ${needle}`);
  }
  const settings = PAGE("Settings");
  for (const needle of ["revoke(t.id)", "ping(h.id)", "toggleDeliveries(h.id)", "remove(h.id)"]) {
    assert.ok(settings.includes(needle), `Settings.jsx behavior intact: ${needle}`);
  }
  assert.ok(PAGE("Team").includes("remove(m)"), "Team.jsx member remove intact");
  assert.ok(PAGE("Checks").includes("setOpen(!getOpen())"), "Checks.jsx expand toggle intact");
  assert.ok(COMP("NotificationTray").includes("markAll"), "NotificationTray.jsx mark-all intact");
});

test("router back-links keep href + A import (no navigation change)", () => {
  for (const [file, href] of [
    ["PullFiles", "/pull/${props.num}"],
    ["PullCommits", "/pull/${num()}"],
    ["CheckDetail", "/checks"],
    ["Team", "/${org()}"],
  ]) {
    const src = PAGE(file);
    assert.ok(src.includes('from "@solidjs/router"'), `${file} still routes via @solidjs/router`);
    assert.ok(src.includes(`class="link`), `${file} keeps the link class on its <A>`);
    assert.ok(src.includes(href), `${file} href intact: ${href}`);
  }
  assert.ok(PAGE("Invitations").includes('href="/explore"'), "Invitations explore-owners link intact");
});

test("rule never fights scoped link styles (class only, no bare `a` rule added)", () => {
  const css = CSS();
  assert.ok(!/^\s*a\s*\{/m.test(css), "no bare `a {` rule — scoped styles (.markdown-body a, .blob-num a, .diff-num a) keep their contexts");
  for (const sel of [".markdown-body a", ".blob-num a", ".diff-num a"]) {
    assert.ok(css.includes(sel), `scoped style intact: ${sel}`);
  }
});

// --- 3. #566/#567 surfaces stay .btn (button actions are never .link) ---

test("composer Cancels stay .btn (the #566 idiom holds)", () => {
  // #587-scoped update: Cancel moved from the per-surface header <p>
  // into CommentComposer's onCancel bottom-row slot — same .btn idiom,
  // one definition instead of two call-site buttons. Intent unchanged:
  // dismiss actions are button-shaped (.btn), never text links (.link).
  const composer = read("../../src/components/CommentComposer.jsx");
  assert.ok(
    composer.includes('<button type="button" class="btn" disabled={getBusy()} onClick={() => props.onCancel()}>'),
    "composer Cancel is a canonical .btn invoking onCancel",
  );
  assert.ok(!composer.includes('class="link'), "no .link anywhere in the composer");
  assert.ok(
    PAGE("Pull").includes("onCancel={() => closeDraft(draftKey(hi(), ri()))}"),
    "conversation draft Cancel still wired (.btn via the composer slot)",
  );
  assert.ok(
    PAGE("PullFiles").includes("onCancel={() => dismissStaged(false)}"),
    "Files-tab staged Cancel still wired (.btn via the composer slot)",
  );
});

test("StagedCard controls stay .btn (the #567 idiom holds)", () => {
  const s = PAGE("Pull");
  const card = s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(!card.includes('class="link"'), "no .link in StagedCard — staged edit/remove are button actions");
  assert.ok(card.includes('class="btn ml-2 px-2 py-0.5 text-xs"'), "staged edit/remove keep the #566 canonical small-btn");
});

// --- 4. lawfulness: no new deps, docs amended ---

test("no new runtime deps", () => {
  const pkg = JSON.parse(read("../../package.json"));
  for (const dep of ["solid-js", "@solidjs/router", "marked", "dompurify"]) {
    assert.ok(pkg.dependencies?.[dep], `still depends on ${dep}`);
  }
  assert.equal(Object.keys(pkg.dependencies ?? {}).length, 4, "exactly the four allowed runtime deps");
});

test("docs/go/12_web_ui.md carries the FIXED (Forgejo #568) amendment", () => {
  const doc = DOC();
  assert.ok(doc.includes("FIXED (Forgejo #568)"), "law-12 amendment present");
  assert.ok(doc.includes("option (a)"), "amendment records the chosen systemic treatment");
});

test("style-guideline names .link as the canonical text-link reference", () => {
  const guide = GUIDE();
  assert.ok(guide.includes("`.link`"), "guideline carries a .link entry");
  assert.ok(guide.includes("#568"), "entry attributes Forgejo #568");
  assert.ok(/never.*\.link|stay `\.btn`, never/.test(guide), "entry states the .btn boundary for button actions");
});
