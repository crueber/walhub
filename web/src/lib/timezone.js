// web/src/lib/timezone.js — IANA timezone picker data (Forgejo #234).
// Headless-testable: no Solid, no DOM — importable in Node.
//
// No tz-database dependency: the list comes from the runtime's own ICU
// (`Intl.supportedValuesOf("timeZone")`), the same source the browser uses
// to interpret the stored zone. The server enforces shape only (length +
// segment charset), so a zone the picker offers is always accepted.

/**
 * timeZones() → string[]. The valid IANA zone names for the picker:
 * `Intl.supportedValuesOf("timeZone")` when the runtime offers it
 * (Node 18+, every evergreen browser), else the `["UTC"]` fallback so the
 * form still renders somewhere without Intl data. Never throws.
 *
 * "UTC" is unioned first: it is valid IANA but missing from some runtimes'
 * lists (Node's ICU omits it; Chromium includes it), and the picker must
 * offer it everywhere — the server shape-check accepts it on every
 * runtime alike.
 */
export function timeZones() {
  const extra = ["UTC"];
  try {
    const list =
      typeof Intl !== "undefined" && typeof Intl.supportedValuesOf === "function"
        ? Intl.supportedValuesOf("timeZone")
        : null;
    if (Array.isArray(list) && list.length > 0) {
      const seen = new Set(extra);
      const out = [...extra];
      for (const z of list) {
        if (typeof z === "string" && z.length > 0 && !seen.has(z)) {
          seen.add(z);
          out.push(z);
        }
      }
      return out;
    }
  } catch {
    // Fall through to the UTC fallback below.
  }
  return extra;
}

/**
 * isValidTimeZone(zone, list?) → boolean. Picker-side validity: membership
 * in the offered list (default: timeZones()). The empty string is valid —
 * it means "unset". Case-sensitive: IANA names are exact (`Europe/Berlin`,
 * never `europe/berlin`).
 */
export function isValidTimeZone(zone, list) {
  if (zone === "" || zone === undefined || zone === null) return true;
  const zones = Array.isArray(list) ? list : timeZones();
  return zones.includes(zone);
}
