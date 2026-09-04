// web/src/lib/sse.js — §2.5 + 08 §4: the shared stream-mount helpers.
// Every streaming component uses these; never EventSource (the SDK's
// fetch-based readers set Accept/auth headers). The page owns the
// lifecycle: run() aborts any previous controller before opening a fresh
// one; the returned cancel() tears down on unmount. Errors set state
// "error", never thrown into the page; mountStreamRetry reschedules with
// capped backoff (1 s → 30 s, reset on open).

import { createSignal } from "solid-js";
import { backoffMs } from "./collab.js";

export { backoffMs };

/**
 * mountStream(open, onFrame, onError?) → { run, cancel, state }.
 * open(signal, emit) starts the stream (via the SDK); emit(frame) forwards a
 * frame to onFrame unless the stream was already aborted/superseded — late
 * frames from a cancelled stream can never overwrite fresh ones.
 * A rejected open sets state "error" (never throws into the page) and
 * calls onError (retry wiring lives in mountStreamRetry).
 */
export function mountStream(open, onFrame, onError) {
  let ctrl = null;
  const [getState, setState] = createSignal("closed");

  const run = () => {
    if (ctrl) ctrl.abort(); // abort the previous stream BEFORE the new one opens
    ctrl = new AbortController();
    const mine = ctrl;
    setState("open");
    Promise.resolve(open(mine.signal, (frame) => {
      if (mine !== ctrl || mine.signal.aborted) return;
      onFrame(frame);
    })).catch(() => {
      if (mine !== ctrl || mine.signal.aborted) return;
      setState("error");
      onError?.();
    });
  };

  const cancel = () => {
    if (ctrl) ctrl.abort();
    ctrl = null;
    setState("closed");
  };

  return { run, cancel, state: getState };
}

/**
 * mountStreamRetry(open, onFrame) → { run, cancel, state }.
 * mountStream plus capped reconnect (08 §4): a failed open reschedules
 * run() with backoffMs (1 s → 30 s); the first frame resets the count.
 * cancel() stops retries (unmount). The page never throws on stream
 * errors; one live stream per component.
 */
export function mountStreamRetry(open, onFrame) {
  let failures = 0;
  let timer = 0;
  let stopped = false;
  const slot = mountStream(
    open,
    (frame) => {
      failures = 0;
      onFrame(frame);
    },
    () => {
      if (stopped) return;
      const ms = backoffMs(failures++);
      clearTimeout(timer);
      timer = setTimeout(() => {
        if (!stopped) slot.run();
      }, ms);
    },
  );
  return {
    run: () => {
      stopped = false;
      failures = 0;
      slot.run();
    },
    cancel: () => {
      stopped = true;
      clearTimeout(timer);
      slot.cancel();
    },
    state: slot.state,
  };
}
