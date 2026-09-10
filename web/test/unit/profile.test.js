// web/test/unit/profile.test.js — owner-profile form helpers (Forgejo #234)
// plus the bio render pipeline the /:owner page reuses: the profile bio
// renders through the SAME marked layer as every other markdown surface
// (render-md.js renderMarkdownHtml — the DOMPurify gate itself is
// browser-only and covered by the real-Chromium pass, same split as
// blob-md.test.js).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  PROFILE_FIELDS,
  emptyProfile,
  normalizeProfile,
  profileSaveBody,
} from "../../src/lib/profile.js";
import { renderMarkdownHtml } from "../../src/lib/render-md.js";

test("PROFILE_FIELDS is exactly the editable set (owner/updated_at/can_edit never save)", () => {
  assert.deepEqual([...PROFILE_FIELDS].sort(), [
    "bio_markdown",
    "display_name",
    "location",
    "timezone",
  ]);
});

test("emptyProfile mirrors the server empty convention ('' = unset)", () => {
  assert.deepEqual(emptyProfile("acme"), {
    owner: "acme",
    display_name: "",
    location: "",
    timezone: "",
    bio_markdown: "",
  });
  assert.equal(emptyProfile().owner, "");
});

test("normalizeProfile coerces a fetched doc into form state", () => {
  const doc = {
    owner: "acme",
    display_name: "Acme Corp",
    location: null,
    timezone: "Europe/Berlin",
    bio_markdown: "# hi",
    updated_at: "2026-09-10T00:00:00Z",
    can_edit: true,
  };
  const form = normalizeProfile(doc, "acme");
  assert.deepEqual(form, {
    owner: "acme",
    display_name: "Acme Corp",
    location: "",
    timezone: "Europe/Berlin",
    bio_markdown: "# hi",
  });
});

test("normalizeProfile falls back to the route owner on junk input", () => {
  assert.deepEqual(normalizeProfile(null, "acme"), emptyProfile("acme"));
  assert.deepEqual(normalizeProfile(undefined, "acme"), emptyProfile("acme"));
  assert.deepEqual(normalizeProfile(42, "acme"), emptyProfile("acme"));
});

test("profileSaveBody picks exactly the editable fields", () => {
  const body = profileSaveBody({
    owner: "acme",
    display_name: "Acme",
    location: "Berlin",
    timezone: "Europe/Berlin",
    bio_markdown: "hi",
    updated_at: "2026-09-10T00:00:00Z",
    can_edit: true,
  });
  assert.deepEqual(body, {
    display_name: "Acme",
    location: "Berlin",
    timezone: "Europe/Berlin",
    bio_markdown: "hi",
  });
  assert.deepEqual(Object.keys(body), PROFILE_FIELDS);
});

test("edit round-trip: normalize → save body → server echo normalizes clean", () => {
  const serverDoc = {
    owner: "acme",
    display_name: "Acme",
    location: "",
    timezone: "",
    bio_markdown: "",
    updated_at: "2026-09-10T00:00:00Z",
  };
  const body = profileSaveBody(normalizeProfile(serverDoc, "acme"));
  assert.deepEqual(body, {
    display_name: "Acme",
    location: "",
    timezone: "",
    bio_markdown: "",
  });
});

test("profile bio renders through the shared markdown layer", () => {
  const html = renderMarkdownHtml("# Acme\n\nWe build *things*.");
  assert.match(html, /<h1[^>]*>Acme<\/h1>/);
  assert.match(html, /<em>things<\/em>/);
});

test("profile bio render escapes raw HTML sources (the gate sees them)", () => {
  // marked passes raw HTML through; the browser DOMPurify gate (same
  // pipeline as thread bodies) drops it. Headless we pin pass-through so a
  // future marked upgrade cannot silently launder scripts past the gate.
  const html = renderMarkdownHtml('<script>alert("x")</script>');
  assert.match(html, /<script>alert/);
});
