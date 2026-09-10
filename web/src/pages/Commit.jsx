// web/src/pages/Commit.jsx — commit detail (§2.8): sha-addressed (immutable)
// fetch, hand-rolled unified-diff parser (lib/diff.js), per-file unified/split
// toggle, per-file anchors, linkified body, grouped trailers, parent links.

import { createEffect, createSignal, onCleanup, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useData, SHA_TTL, EMPTY_REPO, isEmptySummary, isEmptyError, isDegradedSummary, summaryOf, reportError } from "../lib/data.js";
import { parsePatchFiles, linkifyBody, groupTrailers, trailerValue } from "../lib/diff.js";
import { DiffBody } from "../components/DiffTable.jsx";
import { CopySha, shortSha } from "../lib/sha.jsx";
import { useRepo } from "./Repo.jsx";
import DateTime from "../components/DateTime.jsx";
import { CheckPill, ContextRows } from "./Checks.jsx";
import { useRole, roleAtLeast } from "../components/perms.jsx";
import { EmptyRepoGuide, DegradedNotice } from "../components/EmptyRepoGuide.jsx";

const fileAnchor = (path) => `f-${path.replace(/[^a-zA-Z0-9_-]/g, "-")}`;

function DiffFile(props) {
  const [getMode, setMode] = createSignal("unified");
  const f = () => props.file;
  const flags = () =>
    [
      f().added ? "added" : null,
      f().deleted ? "deleted" : null,
      f().isBinary ? "binary" : null,
      f().oldPath && !f().added && !f().deleted ? `renamed from ${f().oldPath}` : null,
    ]
      .filter(Boolean)
      .join(", ");
  const active = (m) => ({
    "!border-emerald-500 !text-emerald-600 dark:!text-emerald-400": getMode() === m,
  });
  return (
    <section class="diff-file card mb-3 overflow-hidden" id={fileAnchor(f().path)}>
      <div class="diff-file-head flex flex-wrap items-baseline gap-2 border-b border-zinc-200 px-3 py-2 dark:border-zinc-800">
        <h3 class="min-w-0 truncate font-mono text-sm font-semibold">{f().path}</h3>
        <Show when={flags()}>
          <span class="muted text-xs">{`(${flags()})`}</span>
        </Show>
        <span class="diffstat tabular ml-auto text-xs">
          <span class="text-emerald-600 dark:text-emerald-400">{`+${f().additions ?? 0}`}</span>{" "}
          <span class="text-red-600 dark:text-red-400">{`−${f().deletions ?? 0}`}</span>
        </span>
        <div class="seg flex gap-1">
          <button type="button" class="pill cursor-pointer" classList={active("unified")} onClick={() => setMode("unified")}>
            Unified
          </button>
          <button type="button" class="pill cursor-pointer" classList={active("split")} onClick={() => setMode("split")}>
            Split
          </button>
        </div>
      </div>
      <div class="diff-holder overflow-x-auto">
        <DiffBody file={f()} mode={getMode()} />
      </div>
    </section>
  );
}

function TrailerTable(props) {
  return (
    <details class="trailers">
      <summary class="pill cursor-pointer select-none" title="Commit trailers (machine-readable footer lines)">
        {props.groups.reduce((n, g) => n + g.trailers.length, 0)} trailers
      </summary>
      <table class="data-table trailers-table mt-2">
        <tbody>
          <For each={props.groups}>
            {({ group, trailers }) => (
              <>
                <tr class="trailers-group">
                  <td colspan={2}>
                    <strong>{group}</strong>
                  </td>
                </tr>
                <For each={trailers}>
                  {(t) => {
                    const v = trailerValue(t.value);
                    return (
                      <tr>
                        <td class="trailer-key align-top font-mono text-xs">{t.key}</td>
                        <td class="trailer-value">
                          <Show when={v.sha}>
                            <A
                              class="sha font-mono text-emerald-700 hover:underline dark:text-emerald-400"
                              href={`/${props.full}/commit/${v.sha}`}
                              title="May not exist here (a CI boundary commit)"
                            >
                              {v.sha.slice(0, 12)}
                            </A>{" "}
                          </Show>
                          <Show when={v.email}>
                            <span>
                              {v.name ?? ""}{" "}
                              <a class="hover:underline" href={`mailto:${v.email}`}>
                                {`<${v.email}>`}
                              </a>{" "}
                            </span>
                          </Show>
                          {v.text ?? ""}
                        </td>
                      </tr>
                    );
                  }}
                </For>
              </>
            )}
          </For>
        </tbody>
      </table>
    </details>
  );
}

function CreateTag(props) {
  // Forgejo #253: lightweight-tag creation at this commit (write-gated —
  // the server is authoritative; the affordance hides entirely otherwise).
  const { role } = useRole(props.full, props.client);
  const [getOpen, setOpen] = createSignal(false);
  const [getName, setName] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getError, setError] = createSignal("");
  const [getCreated, setCreated] = createSignal(null);

  const submit = async (e) => {
    e.preventDefault();
    const name = getName().trim();
    if (!name) {
      setError("Give the tag a name.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const tag = await props.client.tagsApi.create({ name, sha: props.sha });
      setCreated(tag);
      setOpen(false);
      setName("");
    } catch (err) {
      setError(String(err?.message ?? err));
      reportError(err, "create-tag");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Show when={roleAtLeast(role(), "write")}>
      <div class="create-tag mt-3 border-t border-zinc-100 pt-2 dark:border-zinc-800/60">
        <Show when={getCreated()} fallback={
          <Show when={getOpen()} fallback={
            <button type="button" class="pill cursor-pointer" onClick={() => { setOpen(true); setError(""); }}>
              Create tag
            </button>
          }>
            <form class="flex flex-wrap items-center gap-2" onSubmit={submit}>
              <label class="text-xs">
                <span class="sr-only">Tag name</span>
                <input
                  type="text"
                  class="input font-mono text-xs"
                  placeholder="v1.0.0"
                  value={getName()}
                  onInput={(e) => setName(e.currentTarget.value)}
                  disabled={getBusy()}
                  aria-label="Tag name"
                />
              </label>
              <button type="submit" class="pill cursor-pointer" disabled={getBusy()}>
                {getBusy() ? "creating…" : "Create lightweight tag"}
              </button>
              <button type="button" class="pill cursor-pointer" onClick={() => { setOpen(false); setError(""); }} disabled={getBusy()}>
                Cancel
              </button>
            </form>
          </Show>
        }>
          {(tag) => (
            <p class="text-xs" role="status">
              <span class="text-emerald-600 dark:text-emerald-400">Tag {tag().name} created.</span>{" "}
              <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${props.full}/releases/new`}>
                Create a release from it
              </A>
            </p>
          )}
        </Show>
        <Show when={getError()}>
          <p class="err-line mt-1 text-sm" role="alert">{getError()}</p>
        </Show>
      </div>
    </Show>
  );
}

function CheckDetails(props) {
  const [getOpen, setOpen] = createSignal(false);
  return (
    <div class="mt-3 border-t border-zinc-100 pt-2 dark:border-zinc-800/60">
      <button type="button" class="flex items-center gap-2 text-sm" onClick={() => setOpen(!getOpen())} aria-label="Toggle check details">
        <CheckPill full={props.full} sha={props.sha} client={props.client} verbose />
        <span class="link text-xs">{getOpen() ? "hide checks" : "checks"}</span>
      </button>
      <Show when={getOpen()}>
        <div class="mt-2">
          <ContextRows full={props.full} sha={props.sha} client={props.client} />
        </div>
      </Show>
    </div>
  );
}

function CommitDetail(props) {
  // Issue #252: sha-addressed view — publish the sha with an empty name so
  // the header pill shows the short sha honestly (remounts per sha via the
  // keyed parent, so the effect below always carries the current sha).
  createEffect(() => {
    if (props.sha) props.setViewed({ name: "", sha: props.sha });
  });
  onCleanup(() => props.setViewed?.(null));
  // Reactive key: @solidjs/router reuses this route component when only
  // :sha changes, so the key must be a getter — a setup-time string would
  // freeze the view on the first sha (#38).
  const [getCommit] = useData(
    () => `commit:${props.full}:${props.sha}`,
    () => {
      // Known-empty suppression (issue #209): no doomed fetch, no toast.
      if (isEmptySummary(summaryOf(props.full))) return Promise.resolve(EMPTY_REPO);
      return props.repoClient.commit(props.sha).catch((err) => {
        if (isEmptyError(err)) return EMPTY_REPO;
        if (err?.notFound && isDegradedSummary(summaryOf(props.full))) return { degraded: true };
        throw err;
      });
    },
    SHA_TTL,
  );
  const parsed = () => {
    const data = getCommit();
    if (!data) return undefined;
    if (data.empty || data.degraded) return data; // sentinels render guide/notice below
    const c = data.commit ?? {};
    return {
      c,
      stats: data.stats ?? [],
      files: parsePatchFiles(data.patch ?? "", c.sha).files,
      groups: groupTrailers(c.trailers),
    };
  };

  return (
    <div class="commit-page">
      <Show when={parsed()} fallback={<p class="muted animate-pulse">loading commit…</p>}>
        {(d) => (
          <>
            <Show when={d().empty}>
              <EmptyRepoGuide full={props.full} summary={props.summary?.()} />
            </Show>
            <Show when={d().degraded}>
              <DegradedNotice full={props.full} cacheKey={`commit:${props.full}:${props.sha}`} />
            </Show>
            <Show when={!d().empty && !d().degraded}>
              {(() => {
          const c = () => d().c;
          const parents = () => c().parents ?? [];
          const full = () => String(c().sha ?? props.sha);
          const totalAdd = () => d().stats.reduce((n, s) => n + (s.additions ?? 0), 0);
          const totalDel = () => d().stats.reduce((n, s) => n + (s.deletions ?? 0), 0);
          return (
            <>
              <nav class="crumbs mb-2 flex flex-wrap items-center gap-x-1.5 text-sm">
                <A
                  class="text-emerald-700 hover:underline dark:text-emerald-400"
                  href={`/${props.full}/commits`}
                >
                  Commits
                </A>
                <span class="muted" aria-hidden="true">/</span>
                <code class="sha font-mono text-xs tabular-nums" title={full()}>{shortSha(full())}</code>
                <CopySha sha={full()} />
              </nav>

              <section class="commit-head card space-y-2.5 p-4">
                <h2 class="text-lg font-semibold leading-snug">{c().subject ?? "(no message)"}</h2>
                <p class="commit-meta muted text-xs tabular-nums">
                  <span>{c().author ?? ""}</span>
                  <Show when={c().author_email}>
                    <span>{` <${c().author_email}>`}</span>
                  </Show>
                  <span>
                    {" · authored "}<DateTime value={c().author_date} />{" · committed "}<DateTime value={c().committer_date ?? c().commit_date} />
                  </span>
                </p>
                <Show when={parents().length > 0}>
                  <p class="commit-parents text-xs tabular-nums">
                    <span class="muted">
                      {parents().length > 1 ? `merge parents (${parents().length}): ` : "parent: "}
                    </span>
                    <For each={parents()}>
                      {(p, i) => (
                        <>
                          <Show when={i() > 0}>
                            <span class="muted">{" · "}</span>
                          </Show>
                          <A
                            class="sha font-mono text-emerald-700 hover:underline dark:text-emerald-400"
                            href={`/${props.full}/commit/${p}`}
                            title={`parent ${p}`}
                          >
                            {shortSha(p)}
                          </A>
                        </>
                      )}
                    </For>
                  </p>
                </Show>
                <Show when={c().body}>
                  {/* linkifyBody is escape-first (lib/diff.js) — same sanctioned
                      innerHTML pattern as the highlighter output in Blob.jsx */}
                  <div class="commit-body markdown-body mt-3" innerHTML={linkifyBody(c().body, `/${props.full}`)} />
                </Show>
                <Show when={d().groups.length}>
                  <div class="mt-3">
                    <TrailerTable full={props.full} groups={d().groups} />
                  </div>
                </Show>
                <p class="muted diffstat mt-3 text-xs tabular-nums">
                  <span class="text-emerald-600 dark:text-emerald-400">{`+${totalAdd()}`}</span>{" "}
                  <span class="text-red-600 dark:text-red-400">{`−${totalDel()}`}</span>
                  {` across ${d().stats.length} file${d().stats.length === 1 ? "" : "s"} (server stats)`}
                </p>
                <CheckDetails full={props.full} sha={String(c().sha ?? props.sha)} client={props.repoClient} />
                <CreateTag full={props.full} sha={String(c().sha ?? props.sha)} client={props.repoClient} />
              </section>

              <Show when={d().files.length > 0}>
                <nav class="file-nav mt-3 flex flex-wrap items-center gap-1.5 text-xs">
                  <span class="muted">files: </span>
                  <For each={d().files}>
                    {(f) => (
                      <a class="pill cursor-pointer hover:no-underline" href={`#${fileAnchor(f.path)}`}>
                        {f.path}
                      </a>
                    )}
                  </For>
                </nav>
              </Show>

              <div class="commit-files mt-3">
                <For each={d().files}>{(f) => <DiffFile file={f} />}</For>
              </div>
            </>
          );
              })()}
            </Show>
          </>
        )}
      </Show>
    </div>
  );
}

export default function Commit() {
  const ctx = useRepo();
  // Keyed on repo+sha: @solidjs/router reuses this component when only :sha
  // changes (parent links, body sha links), so a sha change must recreate
  // the detail view (resets per-file unified/split toggles with it). The
  // keyed child MUST take the key as its argument: Show returns a no-arg
  // closure as-is (same reference), so `keyed` + `{() => …}` never remounts
  // and the page sticks on the first sha (#38).
  return (
    <Show when={`${ctx.full}:${ctx.sha}`} keyed>
      {(_key) => <CommitDetail full={ctx.full} sha={ctx.sha} repoClient={ctx.repoClient} summary={ctx.summary} setViewed={ctx.setViewed} />}
    </Show>
  );
}
