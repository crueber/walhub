// web/src/components/collab.jsx — 08 §4 page wiring for the ONE repo
// collaboration stream.
//
// useCollabStream(full, client, kinds?, accept?) mounts one connection
// per page via mountStreamRetry (capped 1 s → 30 s reconnect, reset on
// open; unmount cancels). Matching frames invalidate data-layer keys
// through invalidateCollab (collect-into-set per tick + promise-cache
// single-flight per key — the storm coalescing). Frames never carry
// full state: the timeline and lists stay the source of truth.

import { createEffect, onCleanup } from "solid-js";
import { invalidateCollab } from "../lib/data.js";
import { mountStreamRetry } from "../lib/sse.js";

/**
 * @param {string|(()=>string)} full "owner/name" (or getter)
 * @param {object} client the repo client (repo.collab.stream)
 * @param {string[]} [kinds] frame kinds this page cares about (all when omitted)
 * @param {(frame: object) => boolean} [accept] extra per-frame filter (e.g. num match)
 */
export function useCollabStream(full, client, kinds, accept) {
  createEffect(() => {
    const f = typeof full === "function" ? full() : full;
    if (!f || !client?.collab?.stream) return;
    const stream = mountStreamRetry(
      (signal) => client.collab.stream((frame) => {
        if (kinds && !kinds.includes(frame?.kind)) return;
        if (accept && !accept(frame)) return;
        invalidateCollab(f, frame);
      }, { signal }),
      () => {},
    );
    stream.run();
    onCleanup(() => stream.cancel());
  });
}
