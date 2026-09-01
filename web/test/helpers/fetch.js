/**
 * Test helpers (12_web_ui.md §5): a fake fetch with scriptable responses and
 * SSE streams, zero npm dependencies.
 */

/**
 * Build a streaming Response whose body is a ReadableStream emitting the
 * given chunks. Node 18+ has Response/ReadableStream/TextEncoder globals.
 *
 * @param {string|Uint8Array[]} chunks strings (or byte chunks) to emit
 * @param {{status?: number, headers?: Object<string,string>}} [init]
 * @returns {Response}
 */
export function sseResponse(chunks, init = {}) {
  const encoder = new TextEncoder();
  const parts = chunks.map((c) => (typeof c === "string" ? encoder.encode(c) : c));
  let i = 0;
  const body = new ReadableStream({
    pull(controller) {
      if (i < parts.length) controller.enqueue(parts[i++]);
      else controller.close();
    },
  });
  return new Response(body, {
    status: init.status ?? 200,
    headers: {
      "content-type": "text/event-stream; charset=utf-8",
      ...(init.headers ?? {}),
    },
  });
}

/**
 * An endless pull-based SSE stream emitting `chunk` forever — for mid-stream
 * abort tests. pull() defers on the macrotask queue: a synchronous pull would
 * resolve every read() within microtasks and starve timers, so abort
 * listeners could never fire.
 *
 * @param {string} chunk frame text to emit forever
 * @param {{onCancel?: () => void}} [opts]
 * @returns {Response}
 */
export function endlessSseResponse(chunk, { onCancel } = {}) {
  const encoder = new TextEncoder();
  let cancelled = false;
  const body = new ReadableStream({
    pull(controller) {
      return new Promise((resolve) => {
        setTimeout(() => {
          if (!cancelled) controller.enqueue(encoder.encode(chunk));
          resolve();
        }, 0);
      });
    },
    cancel() {
      cancelled = true;
      onCancel?.();
    },
  });
  return new Response(body, { status: 200, headers: { "content-type": "text/event-stream; charset=utf-8" } });
}

/**
 * Build a JSON Response.
 * @param {any} value
 * @param {{status?: number, headers?: Object<string,string>}} [init]
 */
export function jsonResponse(value, init = {}) {
  return new Response(value === null ? "" : JSON.stringify(value), {
    status: init.status ?? 200,
    headers: { "content-type": "application/json", ...(init.headers ?? {}) },
  });
}

/**
 * Build a plain-text (error) Response — the server sends non-2xx bodies as
 * plain text (07_api.md §2).
 */
export function textResponse(text, status = 500) {
  return new Response(text, {
    status,
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}

/** An opaque-redirect Response shape as reported by `redirect: "manual"`. */
export function opaqueRedirectResponse() {
  return { status: 0, statusText: "", ok: false, type: "opaqueredirect", headers: new Headers(), text: async () => "", body: null };
}

/**
 * A fake fetch that records calls and serves from a handler. The handler
 * receives ({url, init, reqIndex}) and returns a Response (or throws).
 *
 * @param {(ctx: {url: string, init: RequestInit, reqIndex: number}) => Response|Promise<Response>} handler
 * @returns {{fetch: typeof fetch, calls: Array<{url: string, init: RequestInit}>}}
 */
export function fakeFetch(handler) {
  const calls = [];
  const fetch = async (url, init = {}) => {
    const ctx = { url, init, reqIndex: calls.length };
    calls.push({ url, init });
    return handler(ctx);
  };
  return { fetch, calls };
}

export function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}