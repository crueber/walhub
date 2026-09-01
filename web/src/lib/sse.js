// web/src/lib/sse.js — §2.5: the shared stream-mount helper. Every streaming
// component uses this; never EventSource (the SDK's fetch-based readers set
// Accept/auth headers). The page owns the lifecycle: run() aborts any previous
// controller before opening a fresh one; the returned cancel() tears down on
// unmount. Errors set state "error", never thrown into the page.

import { createSignal } from "./reactive.js";

/**
 * mountStream(open, onFrame) → { run, cancel, state }.
 * open(signal, emit) starts the stream (via the SDK); emit(frame) forwards a
 * frame to onFrame unless the stream was already aborted/superseded — late
 * frames from a cancelled stream can never overwrite fresh ones.
 */
export function mountStream(open, onFrame) {
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
    });
  };

  const cancel = () => {
    if (ctrl) ctrl.abort();
    ctrl = null;
    setState("closed");
  };

  return { run, cancel, state: getState };
}
