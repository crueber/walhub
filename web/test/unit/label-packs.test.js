import { test } from "node:test";
import assert from "node:assert/strict";

import {
  GITHUB_LABEL_PACK,
  GITLAB_LABEL_PACK,
  LABEL_PACKS,
  missingFromPack,
} from "../../src/lib/label-packs.js";

const HEX6 = /^[0-9a-fA-F]{6}$/;

for (const pack of [GITHUB_LABEL_PACK, GITLAB_LABEL_PACK]) {
  test(`${pack.id}: 8 labels, no case-insensitive duplicate names`, () => {
    assert.equal(pack.labels.length, 8);
    const lower = pack.labels.map((l) => l.name.toLowerCase());
    assert.equal(new Set(lower).size, lower.length);
  });

  test(`${pack.id}: names/colors/descriptions satisfy the 02 §3.1 server validation`, () => {
    for (const l of pack.labels) {
      assert.ok(l.name.length >= 1 && l.name.length <= 64, `name bounds: ${l.name}`);
      assert.match(l.color, HEX6, `6-hex color: ${l.name}`);
      assert.ok((l.description ?? "").length <= 200, `description bounds: ${l.name}`);
    }
  });
}

test("packs registry holds exactly the two packs", () => {
  assert.deepEqual(
    LABEL_PACKS.map((p) => p.id),
    ["github", "gitlab"],
  );
});

test("missingFromPack returns the full pack when nothing exists", () => {
  assert.deepEqual(missingFromPack(GITHUB_LABEL_PACK.labels, []), GITHUB_LABEL_PACK.labels);
  assert.deepEqual(missingFromPack(GITHUB_LABEL_PACK.labels, undefined), GITHUB_LABEL_PACK.labels);
});

test("missingFromPack skips existing names case-insensitively", () => {
  const missing = missingFromPack(GITHUB_LABEL_PACK.labels, ["Bug", "WONTFIX"]);
  assert.deepEqual(
    missing.map((l) => l.name),
    GITHUB_LABEL_PACK.labels.map((l) => l.name).filter((n) => n !== "bug" && n !== "wontfix"),
  );
});

test("missingFromPack is empty when the whole pack exists", () => {
  assert.deepEqual(
    missingFromPack(
      GITLAB_LABEL_PACK.labels,
      GITLAB_LABEL_PACK.labels.map((l) => l.name.toUpperCase()),
    ),
    [],
  );
});
