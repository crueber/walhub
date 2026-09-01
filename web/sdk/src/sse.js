/**
 * SSE envelope + frame parser (12_web_ui.md §1.4, wire format in 07_api.md §6/§7).
 *
 * Shared: the SPA's data layer imports this same parser (one parser rule,
 * 12_web_ui.md §1.0). Plain fetch only — never `EventSource` (it cannot set
 * Accept/auth headers).
 */
import { ReposError } from "./errors.js";

/**
 * Parse one SSE frame (the text between blank lines) into `{event, data}`.
 * Comment lines (`: keepalive`, the `: walgit` opener) are ignored. `data:`
 * lines are joined with `\n` and JSON-decoded when they parse; otherwise the
 * raw string is kept. CR is tolerated everywhere.
 *
 * @param {string} frame raw frame text (no trailing blank line)
 * @returns {{event: string, data: any}}
 */
export function parseFrame(frame) {
  let event = "message";
  const dataLines = [];
  for (const rawLine of frame.split("\n")) {
    const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
    if (line === "" || line.startsWith(":")) continue;
    const idx = line.indexOf(":");
    const field = idx === -1 ? line : line.slice(0, idx);
    let value = idx === -1 ? "" : line.slice(idx + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") event = value;
    else if (field === "data") dataLines.push(value);
  }
  const raw = dataLines.join("\n");
  let data = raw;
  if (raw !== "") {
    try {
      data = JSON.parse(raw);
    } catch {
      // keep the raw string (the server always sends JSON; be lenient anyway)
    }
  }
  return { event, data };
}

/**
 * Read an SSE response body, invoking `onFrame(parsedFrame)` for every frame.
 * Frames split on `\n\n` (CR tolerated); the remainder stays buffered. The
 * reader is ALWAYS closed in a `finally` (§1.6: the SDK owns the reader and
 * closes it). Throws `ReposError(499, "aborted")` when the signal fires or the
 * fetch aborts mid-read. A throwing `onFrame` propagates after the reader is
 * closed.
 *
 * @param {Response} response a 2xx response with `content-type: text/event-stream`
 * @param {(frame: {event: string, data: any}) => void} onFrame
 * @param {{signal?: AbortSignal}} [opts] honor `signal` at every read (§1.4
 *   step 5): abort → ReposError(499, "aborted"). Needed because a ReadableStream
 *   body is not inherently tied to the fetch signal (fake/hand-rolled streams).
 * @returns {Promise<void>} resolves when the stream ends
 */
export async function readSse(response, onFrame, { signal } = {}) {
  const body = response.body;
  if (!body) return;
  const reader = body.getReader();
  try {
    // Rejects with 499 on abort; consumed by the race below. A no-op catch
    // keeps a late/pre-abort rejection from going unhandled.
    let signalReject;
    const abort = signal
      ? new Promise((_, reject) => {
          signalReject = (e) => reject(e);
          signal.addEventListener("abort", () => reject(new ReposError(499, "aborted")), { once: true });
        })
      : null;
    if (abort) abort.catch(() => {});
    const decoder = new TextDecoder();
    let buf = "";
    for (;;) {
      if (signal?.aborted) throw new ReposError(499, "aborted");
      const { done, value } = await (abort ? Promise.race([reader.read(), abort]) : reader.read());
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let m;
      while ((m = /\r?\n\r?\n/.exec(buf)) !== null) {
        const frame = buf.slice(0, m.index);
        buf = buf.slice(m.index + m[0].length);
        if (frame.trim()) onFrame(parseFrame(frame));
      }
    }
    buf += decoder.decode();
    if (buf.trim()) onFrame(parseFrame(buf));
  } catch (err) {
    if (err instanceof ReposError && err.status === 499) throw err;
    if (err && (err.name === "AbortError" || err.name === "TimeoutError")) {
      throw new ReposError(499, "aborted");
    }
    throw err;
  } finally {
    try {
      await reader.cancel();
    } catch {
      // already closed / errored — nothing to release
    }
  }
}