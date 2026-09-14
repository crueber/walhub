// web/test/unit/feature-pills-522.test.js — Forgejo #522: the
// Star/Watch/Fork pills carry a disabled affordance while their flag is
// off, per the #447 idiom (counts stay left of label; the reason rides
// the hover/focus title). Un-star/un-watch always stay enabled (a
// retained star/watch keeps its working button); the flip guards
// belt-and-braces the write too (the API check is the rule). No DOM:
// JSX pinned as source text, mirroring header-pills-447.test.js.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPO = srcOf("../../src/pages/Repo.jsx");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("toggles read the shared summary (zero new requests)", () => {
  assert.ok(REPO.includes("<StarToggle repo={repoClient} summary={getSummary} />"), "shell passes the shared summary to StarToggle");
  assert.ok(REPO.includes("<WatchToggle repo={repoClient} summary={getSummary} />"), "shell passes the shared summary to WatchToggle");
  assert.ok(REPO.includes('import { isFeatureDisabled } from "../lib/repoFeatures.js"'), "shell imports the fail-open flag gate");
});

test("Star pill: disabled affordance, retained star stays enabled", () => {
  const star = block(REPO, "function StarToggle(props)", "function TasksOverlay");
  // The #447 idiom survives: count left of label on the one button.
  assert.ok(star.includes("{s().stars ?? 0} Star"), "Star still reads {n} Star");
  // Disabled affordance: native disabled + reason title + aria.
  assert.ok(star.includes("disabled={off()}"), "the button disables while starring is off");
  assert.ok(star.includes("Starring is disabled for this repository"), "the reason rides the hover/focus title");
  assert.ok(star.includes("aria-disabled={off() || undefined}"), "the disabled state is exposed to AT");
  // …but only when the viewer hasn't starred: a retained star keeps
  // the enabled Unstar button (unstar always works).
  assert.ok(
    star.includes('isFeatureDisabled(props.summary?.(), "star") && !s().viewer?.starred'),
    "the gate spares a retained star (counts stay, unstar works)",
  );
  // Belt-and-braces: the write never fires while disabled (the API
  // check is the rule; the pill is the affordance).
  assert.ok(star.includes('if (isFeatureDisabled(props.summary?.(), "star") && !cur.viewer?.starred) return;'), "flip no-ops while disabled");
});

test("Watch pill: disabled affordance, retained watch stays enabled", () => {
  const watch = block(REPO, "function WatchToggle(props)", "function RefPicker");
  assert.ok(watch.includes("{w().watchers ?? 0} Watch"), "Watch still reads {n} Watch");
  assert.ok(watch.includes("disabled={off()}"), "the button disables while watching is off");
  assert.ok(watch.includes("Watching is disabled for this repository"), "the reason rides the hover/focus title");
  assert.ok(watch.includes("aria-disabled={off() || undefined}"), "the disabled state is exposed to AT");
  assert.ok(
    watch.includes('isFeatureDisabled(props.summary?.(), "watch") && !w().watching'),
    "the gate spares a retained watch (fan-out untouched, unwatch works)",
  );
  assert.ok(watch.includes("if (isFeatureDisabled(props.summary?.(), \"watch\") && !cur.watching) return;"), "flip no-ops while disabled");
});

test("Fork pill: count link stays, CTA label disables with a reason", () => {
  const pill = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "<CloneMenu");
  // The count still links to the network page (existing forks stay
  // browsable, counts intact) while the composer CTA disables.
  assert.ok(pill.includes('href={`/${full()}/forks`}'), "the count link (network page) survives the toggle");
  assert.ok(pill.includes('isFeatureDisabled(s(), "forks")'), "the label gates on the shared summary flag");
  assert.ok(pill.includes("Forking is disabled for this repository"), "the reason rides the hover/focus title");
  assert.ok(pill.includes('aria-disabled="true"'), "the disabled label is exposed to AT");
  // #447 order holds in both states: count left of label.
  const countAt = pill.indexOf("{s().forks ?? 0}");
  const disabledAt = pill.indexOf("Forking is disabled for this repository");
  const labelAt = pill.indexOf("href={forkHref()}");
  assert.ok(countAt !== -1 && countAt < disabledAt && countAt < labelAt, "count stays left of both the disabled label and the composer link");
});
