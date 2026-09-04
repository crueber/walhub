// web/src/pages/PullNew.jsx — route "/:owner/:name/pulls/new" (08 §1):
// open a pull request (write+). Head/base pickers stream refs over SSE
// via repo.refStream (the §2.6 picker pattern: 150 ms debounce, abort
// the in-flight stream on every keystroke, rows keyed by name).

import { createSignal, For, Show, onCleanup } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { reportError } from "../lib/data.js";
import { mountStream } from "../lib/sse.js";
import { roleAtLeast } from "../components/perms.jsx";
import { useRole } from "../components/perms.jsx";

function RefSelect(props) {
  const [getQuery, setQuery] = createSignal("");
  const [getOptions, setOptions] = createSignal([]);
  const [getOpen, setOpen] = createSignal(false);
  let debounce = 0;

  const stream = mountStream(
    (signal, emit) => props.repo.refStream("branches", { q: getQuery(), n: 50 }, emit, { signal }),
    (ref) => setOptions((list) => [...list.filter((r) => r.name !== ref.name), ref]),
  );
  const search = (q) => {
    setQuery(q);
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      setOptions([]);
      stream.run();
    }, 150);
  };
  onCleanup(() => {
    clearTimeout(debounce);
    stream.cancel();
  });

  return (
    <div class="relative">
      <input
        class="input w-full font-mono text-sm"
        placeholder={props.placeholder}
        value={props.value()}
        onInput={(e) => {
          props.onPick(e.target.value);
          search(e.target.value);
          setOpen(true);
        }}
        onFocus={() => {
          setOptions([]);
          stream.run();
          setOpen(true);
        }}
        onBlur={() => setTimeout(() => setOpen(false), 150)}
        onKeyDown={(e) => {
          if (e.key === "Escape") {
            setOpen(false);
            stream.cancel();
          }
        }}
        aria-label={props.label}
      />
      <Show when={getOpen() && getOptions().length > 0}>
        <ul class="ref-list absolute z-10 max-h-48 w-full overflow-y-auto" role="listbox">
          <For each={getOptions()}>
            {(r) => (
              <li>
                <button
                  type="button"
                  class="ref-item w-full text-left font-mono text-xs"
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => {
                    props.onPick(r.name);
                    setOpen(false);
                    stream.cancel();
                  }}
                >
                  {r.name}
                </button>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </div>
  );
}

export default function PullNew() {
  const ctx = useRepo();
  const navigate = useNavigate();
  const { role } = useRole(ctx.full, ctx.repoClient);
  const [getTitle, setTitle] = createSignal("");
  const [getBody, setBody] = createSignal("");
  const [getBase, setBase] = createSignal("refs/heads/main");
  const [getHead, setHead] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);

  const open = async (e) => {
    e.preventDefault();
    if (getBusy() || !getTitle().trim() || !getHead().trim()) return;
    setBusy(true);
    try {
      const res = await ctx.repoClient.pulls.open({
        title: getTitle().trim(),
        body: getBody().trim() || undefined,
        base_ref: getBase().trim(),
        head_ref: getHead().trim(),
      });
      const num = res.thread?.num ?? res.pr?.num;
      navigate(`/${ctx.full}/pull/${num}`);
    } catch (err) {
      reportError(err, "pull-open");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="mx-auto max-w-2xl">
      <h2 class="mb-3 text-lg font-semibold">New pull request</h2>
      <Show when={role() === null} fallback={
        <Show when={roleAtLeast(role(), "write")} fallback={
          <p class="card text-sm">opening pull requests needs the write role — your role: {role() ?? "none"}.</p>
        }>
          <form class="card grid gap-3 p-4" onSubmit={open}>
            <label class="field">
              <span>Title</span>
              <input class="input" value={getTitle()} onInput={(e) => setTitle(e.target.value)} aria-label="title" />
            </label>
            <label class="field">
              <span>Base ref</span>
              <RefSelect repo={ctx.repoClient} value={getBase} onPick={setBase} label="base ref" placeholder="refs/heads/main" />
            </label>
            <label class="field">
              <span>Head ref</span>
              <RefSelect repo={ctx.repoClient} value={getHead} onPick={setHead} label="head ref" placeholder="refs/heads/feature" />
            </label>
            <label class="field">
              <span>Body (optional)</span>
              <textarea class="input min-h-24 font-mono text-sm" value={getBody()} onInput={(e) => setBody(e.target.value)} aria-label="body" />
            </label>
            <div>
              <button type="submit" class="btn primary" disabled={getBusy() || !getTitle().trim() || !getHead().trim()}>
                {getBusy() ? "Opening…" : "Open pull request"}
              </button>
            </div>
          </form>
        </Show>
      }>
        <p class="card text-sm">sign in to open a pull request.</p>
      </Show>
    </div>
  );
}
