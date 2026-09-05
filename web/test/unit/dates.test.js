// web/test/unit/dates.test.js — issue #133: app-wide date display tiers.
// fmtDate: just-now / minutes / hours / days+ordinal / absolute; ordinals
// incl. the 11/12/13 edge; future + invalid + missing inputs. fmtDateTitle:
// local wall-time shape + zone suffix, invalid passthrough.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fmtDate, fmtDateTitle, ordinal } from "../../src/lib/format.js";

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

test("day tier carries relative + ordinal day + month name", () => {
  const months = [
    "January", "February", "March", "April", "May", "June",
    "July", "August", "September", "October", "November", "December",
  ];
  const dayAge = (ms) => {
    const got = fmtDate(agoIso(ms));
    const dd = new Date(Date.now() - ms);
    assert.match(got, / ago - \d+(st|nd|rd|th) of [A-Z][a-z]+$/);
    assert.ok(got.includes(`${ordinal(dd.getDate())} of ${months[dd.getMonth()]}`), got);
    return got;
  };
  assert.ok(dayAge(DAY).startsWith("1 day ago"), fmtDate(agoIso(DAY)));
  assert.ok(fmtDate(agoIso(3 * DAY)).startsWith("3 days ago - "), fmtDate(agoIso(3 * DAY)));
  dayAge(3 * DAY);
  assert.ok(fmtDate(agoIso(30 * DAY)).startsWith("30 days ago - "), fmtDate(agoIso(30 * DAY)));
  dayAge(30 * DAY);
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
  assert.match(fmtDate(agoIso(3 * DAY)), /^3 days ago - /);
  assert.match(fmtDate(agoIso(28 * DAY)), /^28 days ago - /);
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
