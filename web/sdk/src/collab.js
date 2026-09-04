/**
 * Collaboration-stream client (docs/features/08 §4): the ONE repo stream.
 *
 * `GET /{o}/{r}/api/collab/stream` (both lanes via the browser-lane
 * rewrite) carries every live collaboration event for a repo — kinds
 * issue|issue_event|pull|review|thread|check|release|access. Frames
 * invalidate data-layer cache keys; they never carry full state (the
 * timeline and lists stay the source of truth).
 *
 * Fetch-based reader only (never EventSource — 12 §2.5). The caller owns
 * the signal, the SDK owns the reader (§1.6).
 */
import { ReposError } from "./errors.js";
import { readSse } from "./sse.js";

/**
 * Attach the collaboration surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachCollab(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);

  repo.collab = {
    /**
     * Live repo stream: `GET …/api/collab/stream` (SSE, read-gated).
     * Calls `onFrame(frame)` per collaboration frame
     * (`{kind, num?, seq?, sha?, tag?, actor?, at, ...}`).
     *
     * Lifetime: the returned promise settles when the stream ends. A
     * clean server EOF or a network error REJECTS (so mountStreamRetry
     * reconnects with backoff); a caller abort resolves quietly. Cancel
     * via `opts.signal` (the primary cancel path — the SDK derives its
     * controller from it and closes the reader in `finally`, §1.6).
     * Drive this through mountStreamRetry (one live stream per page).
     *
     * @param {(frame: import("./types.js").CollabFrame) => void} onFrame
     * @returns {Promise<void>} settles on stream end
     */
    stream: async (onFrame, opts = {}) => {
      const controller = client._controller(opts?.signal);
      const req = client._request(p("/collab/stream"), {
        headers: { Accept: "text/event-stream" },
        sse: false,
      });
      const res = await client._send(req, controller);
      if (!res.ok) {
        let text = "";
        try {
          text = await res.text();
        } catch {
          /* body unreadable */
        }
        throw new ReposError(res.status, text.trim() || `stream failed`, req.url);
      }
      let cleanEOF = false;
      try {
        await readSse(
          res,
          (frame) => {
            if (frame?.event && frame?.data && typeof frame.data === "object") {
              onFrame?.({ kind: frame.event, ...frame.data });
            }
          },
          { signal: controller.signal },
        );
        cleanEOF = true;
      } finally {
        controller.abort();
      }
      // The server never closes a live stream (keepalives every 10 s):
      // a clean EOF is a dropped connection — reject so the page
      // reconnects instead of staring at a silent timeline. A caller
      // abort rejects as 499 from readSse above, never here.
      if (cleanEOF && !opts?.signal?.aborted) {
        throw new ReposError(502, "stream ended", req.url);
      }
    },
  };
}

export { readSse };
