// web/test/unit/dates.test.js — issue #133: app-wide date display tiers.
// fmtDate: just-now / minutes / hours / relative-only days (#312) / absolute;
// ordinals incl. the 11/12/13 edge; future + invalid + missing inputs.
// fmtDateTitle: local wall-time shape + zone suffix, calendar-date prefix in
// the 1–30-day window (#312), invalid passthrough.
import { test } from "node:test";
import assert from "node:assert/strict";
import { dateTimeAttr, fmtDate, fmtDateTitle, ordinal } from "../../src/lib/format.js";

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

const agoIso = (ms) => new Date(Date.now() - ms).toISOString();

test("under a minute renders just now", () => {
  assert.equal(fmtDate(agoIso(0)), "just now");
  assert.equal(fmtDate(agoIso(30_000)), "just now");
  assert.equal(fmtDate(agoIso(59_000)), "just now");
});

test("minute boundary: 59s just now, 60s one minute", () => {
  assert.equal(fmtDate(agoIso(59 * 1000)), "just now");
  assert.equal(fmtDate(agoIso(60 * 1000)), "1 minute ago");
  assert.equal(fmtDate(agoIso(13 * MIN)), "13 minutes ago");
  assert.equal(fmtDate(agoIso(59 * MIN)), "59 minutes ago");
});

test("hour boundary: 1h singular, 2h and 23h plural", () => {
  assert.equal(fmtDate(agoIso(HOUR)), "1 hour ago");
  assert.equal(fmtDate(agoIso(2 * HOUR)), "2 hours ago");
  assert.equal(fmtDate(agoIso(23 * HOUR)), "23 hours ago");
});

test("day tier is relative only, no ordinal suffix (issue #312)", () => {
  assert.equal(fmtDate(agoIso(DAY)), "1 day ago");
  assert.equal(fmtDate(agoIso(3 * DAY)), "3 days ago");
  assert.equal(fmtDate(agoIso(28 * DAY)), "28 days ago");
  assert.equal(fmtDate(agoIso(30 * DAY)), "30 days ago");
  for (const ms of [DAY, 3 * DAY, 30 * DAY]) {
    assert.ok(!fmtDate(agoIso(ms)).includes(" - "), fmtDate(agoIso(ms)));
  }
});

test("31 days and beyond render absolute month + ordinal + year", () => {
  for (const ms of [31 * DAY, 100 * DAY, 365 * DAY]) {
    const dd = new Date(Date.now() - ms);
    const months = [
      "January", "February", "March", "April", "May", "June",
      "July", "August", "September", "October", "November", "December",
    ];
    assert.equal(
      fmtDate(agoIso(ms)),
      `${months[dd.getMonth()]} ${ordinal(dd.getDate())}, ${dd.getFullYear()}`,
    );
  }
});

test("one year ago is absolute", () => {
  assert.match(fmtDate(agoIso(365 * DAY)), /^[A-Z][a-z]+ \d+(st|nd|rd|th), \d{4}$/);
});

test("issue examples render in the right tier", () => {
  assert.equal(fmtDate(agoIso(13 * MIN)), "13 minutes ago");
  assert.equal(fmtDate(agoIso(2 * HOUR)), "2 hours ago");
  assert.match(fmtDate(agoIso(3 * DAY)), /^3 days ago$/);
  assert.match(fmtDate(agoIso(28 * DAY)), /^28 days ago$/);
  assert.match(fmtDate(agoIso(400 * DAY)), /^[A-Z][a-z]+ \d+(st|nd|rd|th), \d{4}$/);
});

test("ordinals incl. the 11/12/13 edge", () => {
  assert.equal(ordinal(1), "1st");
  assert.equal(ordinal(2), "2nd");
  assert.equal(ordinal(3), "3rd");
  assert.equal(ordinal(4), "4th");
  assert.equal(ordinal(11), "11th");
  assert.equal(ordinal(12), "12th");
  assert.equal(ordinal(13), "13th");
  assert.equal(ordinal(21), "21st");
  assert.equal(ordinal(22), "22nd");
  assert.equal(ordinal(23), "23rd");
  assert.equal(ordinal(24), "24th");
  assert.equal(ordinal(31), "31st");
  assert.equal(ordinal(111), "111th");
  assert.equal(ordinal(112), "112th");
  assert.equal(ordinal(113), "113th");
});

test("future timestamps clamp to just now", () => {
  assert.equal(fmtDate(new Date(Date.now() + MIN).toISOString()), "just now");
  assert.equal(fmtDate(new Date(Date.now() + DAY).toISOString()), "just now");
});

test("missing and invalid inputs keep the old fallbacks", () => {
  assert.equal(fmtDate(""), "");
  assert.equal(fmtDate(null), "");
  assert.equal(fmtDate(undefined), "");
  assert.equal(fmtDate("not-a-date"), "not-a-date");
  assert.equal(fmtDateTitle(""), "");
  assert.equal(fmtDateTitle(null), "");
  assert.equal(fmtDateTitle(undefined), "");
  assert.equal(fmtDateTitle("not-a-date"), "not-a-date");
});

test("title is the local wall time plus a real zone name", () => {
  const iso = "2025-03-04T08:23:00.000Z";
  const d = new Date(iso);
  const pad = (n) => String(n).padStart(2, "0");
  const want = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
    `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const got = fmtDateTitle(iso);
  assert.match(got, /^\d{4}-\d{2}-\d{2} \d{2}:\d{2} \S+$/);
  assert.ok(got.startsWith(want), `title ${got} should start with local wall time ${want}`);
  assert.ok(!got.endsWith("Z"), "title must be local, never UTC-suffixed");
});

test("title prepends the calendar date in the 1-30-day window (issue #312)", () => {
  const months = [
    "January", "February", "March", "April", "May", "June",
    "July", "August", "September", "October", "November", "December",
  ];
  const pad = (n) => String(n).padStart(2, "0");
  const titleAt = (ms) => {
    const now = Date.now();
    const iso = new Date(now - ms).toISOString();
    return { got: fmtDateTitle(iso, now), d: new Date(iso) };
  };
  // Boundary pins: exactly 1 day and 30 days carry the prefix, 31 days does not.
  for (const ms of [DAY, 3 * DAY, 30 * DAY]) {
    const { got, d } = titleAt(ms);
    const wall = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
      `${pad(d.getHours())}:${pad(d.getMinutes())}`;
    const want = `${months[d.getMonth()]} ${ordinal(d.getDate())} · ${wall}`;
    assert.ok(got.startsWith(want), `title ${got} should start with ${want}`);
    assert.ok(!got.endsWith("Z"), "title must be local, never UTC-suffixed");
  }
  // 31+ days keep the plain wall-time title (visible text already absolute).
  {
    const { got, d } = titleAt(31 * DAY);
    const wall = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
      `${pad(d.getHours())}:${pad(d.getMinutes())}`;
    assert.ok(got.startsWith(wall), `title ${got} should start with local wall time ${wall}`);
    assert.ok(!got.includes("·"), `31-day title ${got} must not carry the calendar prefix`);
  }
  // Under a day keeps the plain wall-time title (no calendar date needed).
  {
    const { got, d } = titleAt(2 * HOUR);
    const wall = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
      `${pad(d.getHours())}:${pad(d.getMinutes())}`;
    assert.ok(got.startsWith(wall), `title ${got} should start with local wall time ${wall}`);
    assert.ok(!got.includes("·"), `sub-day title ${got} must not carry the calendar prefix`);
  }
});

test("dateTimeAttr normalizes to UTC ISO, never throws", () => {
  assert.equal(dateTimeAttr("2025-03-04T08:23:00.000Z"), "2025-03-04T08:23:00.000Z");
  assert.equal(dateTimeAttr("2025-03-04T10:23:00+02:00"), "2025-03-04T08:23:00.000Z");
  assert.equal(dateTimeAttr(new Date("2025-03-04T08:23:00.000Z")), "2025-03-04T08:23:00.000Z");
  assert.equal(dateTimeAttr(Date.parse("2025-03-04T08:23:00.000Z")), "2025-03-04T08:23:00.000Z");
  assert.equal(dateTimeAttr("not-a-date"), "not-a-date");
});
