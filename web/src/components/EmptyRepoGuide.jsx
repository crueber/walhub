// web/src/components/EmptyRepoGuide.jsx — issue #209 guided empty state +
// degraded inline notice. Thin DOM over the pure lib builders (clone.js) and
// the data-layer predicates (data.js): no fetch here, ever — the guide takes
// the shell's summary (verbatim server clone_url, issue #124 rule) and the
// notice takes a retry that invalidates the cached degraded sentinel.

import { createSignal, Show, onCleanup } from "solid-js";
import { A } from "@solidjs/router";
import { httpsCloneUrl, httpProtoLabel, sshCloneUrl, copyText } from "../lib/clone.js";
import { invalidate } from "../lib/data.js";

function CopyButton(props) {
  const [getCopied, setCopied] = createSignal("");
  let timer = 0;
  onCleanup(() => clearTimeout(timer));
  const copy = async () => {
    setCopied((await copyText(props.text())) ? "copied" : "failed");
    clearTimeout(timer);
    timer = setTimeout(() => setCopied(""), 2000);
  };
  return (
    <span class="inline-flex items-center gap-1">
      <button type="button" class="btn shrink-0 px-2 py-1 text-xs" onClick={copy}>
        Copy
      </button>
      <span role="status" aria-live="polite" class="muted text-xs">
        {getCopied() === "copied" ? "copied" : getCopied() === "failed" ? "copy failed — press Ctrl+C" : ""}
      </span>
    </span>
  );
}

/**
 * EmptyRepoGuide — the Code-tab block for unborn repos (summary health
 * "empty"): verbatim clone URL + `git remote add` / `git push -u origin
 * main` with copy buttons and the CloneMenu protocol-toggle parity. Static
 * commands only — no recipes fetch.
 */
export function EmptyRepoGuide(props) {
  const [getProto, setProto] = createSignal("http");
  // Verbatim server URL first (issue #124: never upgrade http→https for
  // display), origin fallback before the summary lands.
  const https = () => httpsCloneUrl(props.summary, props.full, location.origin);
  const url = () => (getProto() === "ssh" ? sshCloneUrl(https(), props.full, location.hostname) : https());
  const remoteCmd = () => `git remote add origin ${url()}`;
  const pushCmd = () => `git push -u origin main`;
  return (
    <section class="empty-guide card space-y-3 p-4" aria-label="push to this empty repository">
      <h2 class="text-lg font-semibold">This repository is empty</h2>
      <p class="muted text-sm">
        Push a branch to fill it — the first push lands directly, nothing to approve.
      </p>
      <div class="flex items-center gap-2">
        <div role="group" aria-label="Clone protocol" class="flex gap-1">
          {(["http", "ssh"]).map((p) => (
            <button
              type="button"
              class="btn px-2 py-1 text-xs"
              classList={{ "btn-active": getProto() === p }}
              aria-pressed={getProto() === p}
              onClick={() => setProto(p)}
            >
              {p === "http" ? httpProtoLabel(https()) : "SSH"}
            </button>
          ))}
        </div>
      </div>
      <div class="space-y-2">
        <div class="flex items-center gap-2">
          <code class="clone-cmd block flex-1 overflow-x-auto rounded bg-zinc-100 px-2 py-1 font-mono text-xs dark:bg-zinc-800">
            {`git clone ${url()}`}
          </code>
          <CopyButton text={() => `git clone ${url()}`} />
        </div>
        <div class="flex items-center gap-2">
          <code class="clone-cmd block flex-1 overflow-x-auto rounded bg-zinc-100 px-2 py-1 font-mono text-xs dark:bg-zinc-800">
            {remoteCmd()}
          </code>
          <CopyButton text={remoteCmd} />
        </div>
        <div class="flex items-center gap-2">
          <code class="clone-cmd block flex-1 overflow-x-auto rounded bg-zinc-100 px-2 py-1 font-mono text-xs dark:bg-zinc-800">
            {pushCmd()}
          </code>
          <CopyButton text={pushCmd} />
        </div>
      </div>
    </section>
  );
}

/**
 * DegradedNotice — inline (never toast) read error for known-degraded repos:
 * the object is missing from the store. Retry invalidates the cached
 * degraded sentinel so a healed object refetches; the WAL tab carries the
 * full audit picture.
 */
export function DegradedNotice(props) {
  const retry = () => {
    if (props.cacheKey) invalidate(props.cacheKey);
    props.onRetry?.();
  };
  return (
    <div
      class="degraded-notice card border-amber-500 p-4 text-sm"
      role="status"
    >
      <p>
        This object is missing from the store — see{" "}
        <A class="hover:underline" href={`/${props.full}/settings#wal`}>
          WAL health
        </A>
        . Reads of present objects are unaffected; pushes of new refs still work.
      </p>
      <Show when={props.cacheKey}>
        <p class="mt-2">
          <button type="button" class="btn px-2 py-1 text-xs" onClick={retry}>
            Retry
          </button>
        </p>
      </Show>
    </div>
  );
}
