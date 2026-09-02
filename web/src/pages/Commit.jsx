// web/src/pages/Commit.jsx — commit detail (§2.8): sha-addressed (immutable)
// fetch, hand-rolled unified-diff parser (lib/diff.js), per-file unified/split
// toggle, per-file anchors, linkified body, grouped trailers, parent links.

import { createSignal, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useData, SHA_TTL } from "../lib/data.js";
import { parsePatchFiles, splitRows, linkifyBody, groupTrailers, trailerValue } from "../lib/diff.js";
import { fmtDate, useRepo } from "./Repo.jsx";

const fileAnchor = (path) => `f-${path.replace(/[^a-zA-Z0-9_-]/g, "-")}`;
const lineClass = (t) => (t === "+" ? "diff-add" : t === "-" ? "diff-del" : "");

function DiffBody(props) {
  return (
    <Show
      when={!props.file.isBinary}
      fallback={
        <table class="diff w-full font-mono text-xs">
          <tbody>
            <tr>
              <td class="muted p-3" colspan={props.mode === "split" ? 2 : 1}>
                Binary file not shown
              </td>
            </tr>
          </tbody>
        </table>
      }
    >
      <table class="diff w-full font-mono text-xs leading-5">
        <tbody>
          <Show
            when={props.mode === "split"}
            fallback={
              <For each={props.file.hunks}>
                {(h) => (
                  <>
                    <tr>
                      <td class="diff-hunk px-3">
                        {`@@ -${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines} @@ ${h.context ?? ""}`}
                      </td>
                    </tr>
                    <For each={h.lines}>
                      {(l) => (
                        <tr>
                          <td class={lineClass(l.t)}>{l.text || " "}</td>
                        </tr>
                      )}
                    </For>
                  </>
                )}
              </For>
            }
          >
            <For each={splitRows([].concat(...props.file.hunks.map((h) => h.lines)))}>
              {(row) => (
                <tr>
                  <td class={row.left ? lineClass(row.left.t) : ""}>
                    {row.left ? row.left.text || " " : " "}
                  </td>
                  <td class={row.right ? lineClass(row.right.t) : ""}>
                    {row.right ? row.right.text || " " : " "}
                  </td>
                </tr>
              )}
            </For>
          </Show>
        </tbody>
      </table>
    </Show>
  );
}

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
      <table class="grid trailers-table mt-2">
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

function CommitDetail(props) {
  const [getCommit] = useData(
    `commit:${props.full}:${props.sha}`,
    () => props.repoClient.commit(props.sha),
    SHA_TTL,
  );
  const parsed = () => {
    const data = getCommit();
    if (!data) return undefined;
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
      <Show when={parsed()} fallback={<p class="muted">loading commit…</p>}>
        {(d) => {
          const c = () => d().c;
          const totalAdd = () => d().stats.reduce((n, s) => n + (s.additions ?? 0), 0);
          const totalDel = () => d().stats.reduce((n, s) => n + (s.deletions ?? 0), 0);
          return (
            <>
              <nav class="crumbs mb-2 text-sm">
                <A
                  class="text-emerald-700 hover:underline dark:text-emerald-400"
                  href={`/${props.full}/commits`}
                >
                  Commits
                </A>
                {" / "}
                <code class="sha font-mono">{String(c().sha ?? props.sha).slice(0, 12)}</code>
              </nav>

              <section class="commit-head card p-4">
                <h2 class="text-lg font-semibold">{c().subject ?? "(no message)"}</h2>
                <div class="muted commit-meta mt-1 text-xs">
                  <span>{c().author ?? ""}</span>
                  <Show when={c().author_email}>
                    <span>{` <${c().author_email}>`}</span>
                  </Show>
                  <span>
                    {` · authored ${fmtDate(c().author_date)} · committed ${fmtDate(c().committer_date ?? c().commit_date)}`}
                  </span>
                  <Show when={(c().parents ?? []).length > 0}>
                    <span>
                      {" · parent "}
                      <For each={c().parents}>
                        {(p) => (
                          <A
                            class="sha font-mono text-emerald-700 hover:underline dark:text-emerald-400"
                            href={`/${props.full}/commit/${p}`}
                          >
                            {String(p).slice(0, 12)}
                          </A>
                        )}
                      </For>
                    </span>
                  </Show>
                </div>
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
                <p class="muted diffstat mt-3 text-xs">
                  <span class="text-emerald-600 dark:text-emerald-400">{`+${totalAdd()}`}</span>{" "}
                  <span class="text-red-600 dark:text-red-400">{`−${totalDel()}`}</span>
                  {` across ${d().stats.length} file${d().stats.length === 1 ? "" : "s"} (server stats)`}
                </p>
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
        }}
      </Show>
    </div>
  );
}

export default function Commit() {
  const ctx = useRepo();
  // Keyed on repo+sha: @solidjs/router reuses this component when only :sha
  // changes (parent links, body sha links), and the fetch is keyed at setup —
  // so a sha change must recreate the detail view.
  return (
    <Show when={`${ctx.full}:${ctx.sha}`} keyed>
      {() => <CommitDetail full={ctx.full} sha={ctx.sha} repoClient={ctx.repoClient} />}
    </Show>
  );
}
