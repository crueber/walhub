// web/test/unit/org-profile.test.js — org-profile form helpers (Forgejo
// #359): field list, empty defaults, fetched-doc normalization, and the
// save body (exactly the five editable fields — avatar/updated_at never
// leak into the PUT).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  ORG_PROFILE_FIELDS,
  emptyOrgProfile,
  normalizeOrgProfile,
  orgSaveBody,
} from "../../src/lib/org-profile.js";

test("ORG_PROFILE_FIELDS matches the owner-profile spelling plus description", () => {
  assert.deepEqual(ORG_PROFILE_FIELDS, ["display_name", "description", "location", "timezone", "bio_markdown"]);
});

test("emptyOrgProfile defaults every field to unset", () => {
  assert.deepEqual(emptyOrgProfile("acme"), {
    org: "acme",
    display_name: "",
    description: "",
    location: "",
    timezone: "",
    bio_markdown: "",
  });
  assert.equal(emptyOrgProfile(null).org, "");
});

test("normalizeOrgProfile coerces a fetched org doc into form state", () => {
  assert.deepEqual(
    normalizeOrgProfile(
      {
        org: "acme",
        display_name: "Acme Corp",
        description: "tag",
        location: "Berlin, DE",
        timezone: "Europe/Berlin",
        bio_markdown: "# hi",
        version: 3,
        avatar_content_type: "image/png",
        avatar_updated_at: "2026-09-12T00:00:00Z",
        updated_at: "2026-09-12T00:00:00Z",
      },
      "acme"
    ),
    {
      org: "acme",
      display_name: "Acme Corp",
      description: "tag",
      location: "Berlin, DE",
      timezone: "Europe/Berlin",
      bio_markdown: "# hi",
    }
  );
  // Missing/null/non-string fields fall back to "" (inputs stay controlled).
  assert.deepEqual(normalizeOrgProfile({ org: "acme", display_name: null, location: 7 }, "acme"), {
    org: "acme",
    display_name: "",
    description: "",
    location: "",
    timezone: "",
    bio_markdown: "",
  });
  assert.deepEqual(normalizeOrgProfile(null, "acme").org, "acme");
});

test("orgSaveBody carries exactly the five editable fields", () => {
  assert.deepEqual(
    orgSaveBody({
      display_name: "Acme",
      description: "d",
      location: "L",
      timezone: "UTC",
      bio_markdown: "b",
      org: "acme",
      version: 9,
      avatar_content_type: "image/png",
      can_edit: true,
    }),
    { display_name: "Acme", description: "d", location: "L", timezone: "UTC", bio_markdown: "b" }
  );
  assert.deepEqual(orgSaveBody(null), {
    display_name: "",
    description: "",
    location: "",
    timezone: "",
    bio_markdown: "",
  });
});
