// web/src/pages/Pull.jsx — route "/:owner/:name/pull/:num" (03 §9 + 04 §8):
// the PR conversation (timeline, comment box, merge box with strategy
// select, mergeable state, and task progress) plus the code-review surface:
// review summary bar, reviews list, reviewers panel, finish-review modal,
// and the diff review surface (line anchors via lib/diff.js
// anchorContextSha — the ONLY §4 hash implementation client-side).
// `pull` frames refresh the header; the merge box polls the pull-merge
// task record while it runs. Review/thread mutations reload via
// invalidate (no polling loops, P7); the `review`/`thread` SSE frames ride
// the collaboration stream once 06 lands it (the server already
// publishes them — see internal/review).

import { createSignal, For, Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useRepo, fmtDate } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { parsePatchFiles, anchorContextSha } from "../lib/diff.js";

function eventText(ev) {
  switch (ev.type) {
    case "opened":
    case "commented":
      return null; // rendered as body
    case "title_changed":
      return `retitled “${ev.from}” → “${ev.to}”`;
    case "state_changed":
      return ev.to === "closed" ? "closed" : "reopened";
    case "merged":
      return `merged as ${(ev.merge_commit_sha ?? "").slice(0, 12)} (${ev.strategy ?? "merge"})`;
    case "head_force_pushed":
      return `head force-pushed ${(ev.from ?? "").slice(0, 12)} → ${(ev.to ?? "").slice(0, 12)}`;
    default:
      return ev.type;
  }
}

function mergeableText(m) {
  if (!m) return "unknown";
  switch (m.state) {
    case "clean":
      return "mergeable";
    case "behind":
      return "mergeable (behind)";
    case "dirty":
      return `conflicts: ${(m.conflicts ?? []).join(", ")}`;
    case "up_to_date":
      return "already merged";
    default:
      return "checking…";
  }
}

function decisionBadge(decision) {
  switch (decision) {
    case "APPROVED":
      return "pill bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200";
    case "CHANGES_REQUESTED":
      return "pill bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200";
    default:
      return "pill bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200";
  }
}

/** Review summary bar (§8): decision badge + reviewer chips (stale derived
 *  client-side from commit_sha != head) + requested chips + unresolved. */
function ReviewSummaryBar(props) {
  const summary = () => props.summary;
  const latest = () => Object.entries(summary()?.latest ?? {});
  return (
    <div class="card" aria-label="Review summary">
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <span class={decisionBadge(summary()?.decision ?? "REVIEW_REQUIRED")}>
          {summary()?.decision ?? "REVIEW_REQUIRED"}
        </span>
        <span class="text-xs text-zinc-500 dark:text-zinc-400">
          {summary()?.approvals ?? 0} approvals · {summary()?.threads_unresolved ?? 0} unresolved threads
        </span>
      </div>
      <div class="flex flex-wrap gap-1.5">
        <For each={latest()} fallback={<span class="text-xs text-zinc-500 dark:text-zinc-400">no reviews yet</span>}>
          {([who, r]) => (
            <span class="pill" title={`${r.state} @ ${String(r.commit_sha ?? "").slice(0, 12)}`}>
              {who} · {r.state}
              <Show when={r.state === "APPROVED" && r.commit_sha !== props.head}>
                <span class="ml-1 font-semibold text-amber-600 dark:text-amber-400">(stale)</span>
              </Show>
            </span>
          )}
        </For>
        <For each={summary()?.requested ?? []}>
          {(who) => <span class="pill opacity-70" title="requested reviewer">{who} · requested</span>}
        </For>
      </div>
    </div>
  );
}

/** Reviews list: cards driven by review_summary + GET reviews. */
function ReviewsList(props) {
  const submitDismiss = async (seq) => {
    const reason = window.prompt("Dismissal reason (recorded on the compensating event):", "stale");
    if (!reason || !reason.trim()) return;
    try {
      await props.client.pulls.reviews.dismiss(props.num, seq, { reason: reason.trim() });
      props.reload();
    } catch (err) {
      reportError(err, "review-dismiss");
    }
  };
  return (
    <div class="card" aria-label="Reviews">
      <h2 class="mb-2 text-sm font-semibold">Reviews</h2>
      <ul class="card-list">
        <For each={props.reviews ?? []} fallback={<li class="text-sm text-zinc-500 dark:text-zinc-400">No reviews yet.</li>}>
          {(rv) => (
            <li class="card">
              <div class="card-meta">
                <span>{rv.by}</span>
                <span class={decisionBadge(rv.state ?? rv.kind)}>{rv.kind === "review_dismissed" ? `dismissed #${rv.dismisses}` : rv.state}</span>
                <span>{fmtDate(rv.at)}</span>
              </div>
              <Show when={rv.kind === "review_dismissed"}>
                <p class="text-sm italic">dismissed review #{rv.dismisses}: {rv.reason}</p>
              </Show>
              <Show when={rv.body}>
                <p class="whitespace-pre-wrap">{rv.body}</p>
              </Show>
              <p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
                {(rv.commit_sha ?? "").slice(0, 12)}
                <Show when={rv.commit_sha && rv.commit_sha !== props.head}>
                  <span class="ml-1 font-semibold text-amber-600 dark:text-amber-400">stale</span>
                </Show>
                <Show when={props.canDismiss && rv.kind === "review"}>
                  <button type="button" class="link ml-2" onClick={() => submitDismiss(rv.seq)}>
                    dismiss
                  </button>
                </Show>
              </p>
            </li>
          )}
        </For>
      </ul>
    </div>
  );
}

/** Reviewers panel: picker fed by review-suggest (150 ms debounce,
 *  abort-on-keystroke per the §2.6 ref-picker pattern). */
function ReviewersPanel(props) {
  const [getQuery, setQuery] = createSignal("");
  const [getOptions, setOptions] = createSignal([]);
  const [getOpen, setOpen] = createSignal(false);
  let debounce = 0;
  let ctrl = null;

  const search = (q) => {
    setQuery(q);
    clearTimeout(debounce);
    if (ctrl) ctrl.abort();
    debounce = setTimeout(async () => {
      ctrl = new AbortController();
      try {
        const { suggestions } = await props.client.pulls.suggest(props.num, q, { signal: ctrl.signal });
        setOptions(suggestions ?? []);
        setOpen(true);
      } catch (err) {
        if (String(err?.message ?? err).toLowerCase().includes("abort")) return;
        reportError(err, "review-suggest");
      }
    }, 150);
  };

  const add = async (who) => {
    setOpen(false);
    try {
      await props.client.pulls.requests.add(props.num, [who]);
      setQuery("");
      setOptions([]);
      props.reload();
    } catch (err) {
      reportError(err, "review-request");
    }
  };

  const remove = async (who) => {
    try {
      await props.client.pulls.requests.remove(props.num, [who]);
      props.reload();
    } catch (err) {
      reportError(err, "review-request");
    }
  };

  return (
    <div class="card" aria-label="Reviewers">
      <h2 class="mb-2 text-sm font-semibold">Reviewers</h2>
      <div class="relative">
        <input
          class="input w-full"
          placeholder="request a reviewer…"
          value={getQuery()}
          onInput={(e) => search(e.target.value)}
          aria-label="Request a reviewer"
        />
        <Show when={getOpen() && getOptions().length > 0}>
          <ul class="ref-list absolute z-10 max-h-48 w-full overflow-y-auto">
            <For each={getOptions()}>
              {(who) => (
                <li>
                  <button type="button" class="ref-item w-full text-left" onClick={() => add(who)}>
                    {who}
                  </button>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </div>
      <ul class="mt-2 flex flex-wrap gap-1.5">
        <For each={props.requested ?? []} fallback={<li class="text-xs text-zinc-500 dark:text-zinc-400">none requested</li>}>
          {(who) => (
            <li class="pill">
              {who}
              <button type="button" class="link ml-1" onClick={() => remove(who)} aria-label={`Remove ${who}`}>
                ×
              </button>
            </li>
          )}
        </For>
      </ul>
    </div>
  );
}

/** Number the display lines of one parsed hunk (hunk.newStart/oldStart
 *  counters over the parsed rows). Returns [{line, t, text, oldNo, newNo}]. */
function numberedLines(hunk) {
  let o = hunk.oldStart;
  let n = hunk.newStart;
  return (hunk.lines ?? []).map((l, i) => {
    const row = { line: l, idx: i, oldNo: null, newNo: null };
    if (l.t === " ") {
      row.oldNo = o++;
      row.newNo = n++;
    } else if (l.t === "-") {
      row.oldNo = o++;
    } else if (l.t === "+") {
      row.newNo = n++;
    }
    return row;
  });
}

/** Derive freshness for one thread against the CURRENT diff: recompute the
 *  §4 hash from the diff — mismatch (or an unlocatable anchor) renders the
 *  thread outdated (collapsed, original line shown), never relocated. */
function threadFreshness(thread, files) {
  const a = thread.anchor ?? {};
  for (const f of files) {
    if (f.path !== a.path) continue;
    for (const h of f.hunks ?? []) {
      const rows = numberedLines(h);
      const idx = rows.findIndex((r) =>
        a.side === "NEW" ? r.newNo === a.new_start : r.oldNo === a.old_start
      );
      if (idx < 0) continue;
      const fresh = anchorContextSha({ path: f.path, lines: h.lines }, { start: idx, count: 1 });
      return { fresh: fresh === a.context_sha, hunk: h, idx };
    }
  }
  return { fresh: false, hunk: null, idx: -1 };
}

/** One file's hunks with line comment affordances + inline thread cards
 *  (unresolved-first; unlocatable/drifted anchors render once, collapsed,
 *  at the file end — never relocated). */
function DiffFile(props) {
  const stageLine = (file, hunk, row) => {
    // A line's comment affordance builds the anchor from the parsed hunk:
    // side/old_*/new_* from hunk counters, path from the file header,
    // commit_sha from the rendered head, context_sha via anchorContextSha.
    const isNew = row.line.t !== "-";
    const anchor = isNew
      ? { path: file.path, side: "NEW", old_start: 0, old_lines: 0, new_start: row.newNo, new_lines: 1, commit_sha: props.head, context_sha: "" }
      : { path: file.path, side: "OLD", old_start: row.oldNo, old_lines: 1, new_start: 0, new_lines: 0, commit_sha: props.head, context_sha: "" };
    anchor.context_sha = anchorContextSha(
      { path: file.path, lines: hunk.lines },
      { start: row.idx, count: 1 }
    );
    const body = window.prompt(`Comment on ${file.path}:${isNew ? row.newNo : row.oldNo} (staged into the finish-review modal):`, "");
    if (body === null || !body.trim()) return;
    props.onStage({ anchor, body: body.trim() });
  };

  // Placement is derived fresh every render: locate each thread's anchor
  // line in the CURRENT file hunks (inline, unresolved-first); anchors
  // that no longer locate fall to the file end, collapsed (outdated).
  const placement = () => {
    const mine = (props.threads ?? []).filter((t) => (t.anchor?.path ?? "") === props.file.path);
    const byKey = new Map();
    const unplaced = [];
    for (const t of mine) {
      const a = t.anchor ?? {};
      const no = a.side === "NEW" ? a.new_start : a.old_start;
      let found = null;
      (props.file.hunks ?? []).forEach((hunk, hi) => {
        if (found) return;
        numberedLines(hunk).forEach((row, ri) => {
          if (found) return;
          if ((row.newNo === no && a.side === "NEW") || (row.oldNo === no && a.side === "OLD")) {
            found = `${hi}:${ri}`;
          }
        });
      });
      const f = threadFreshness(t, props.files);
      t._fresh = f.fresh;
      if (found) {
        if (!byKey.has(found)) byKey.set(found, []);
        byKey.get(found).push(t);
      } else {
        unplaced.push(t);
      }
    }
    for (const list of byKey.values()) list.sort((x, y) => Number(x.resolved) - Number(y.resolved));
    unplaced.sort((x, y) => Number(x.resolved) - Number(y.resolved));
    return { byKey, unplaced };
  };

  const threadsAt = (hi, ri) => placement().byKey.get(`${hi}:${ri}`) ?? [];

  return (
    <div class="card" aria-label={`Diff ${props.file.path}`}>
      <h3 class="mb-2 font-mono text-sm font-semibold">{props.file.path}</h3>
      <For each={props.file.hunks ?? []}>
        {(hunk, hi) => {
          const rows = numberedLines(hunk);
          return (
            <div class="mb-3 overflow-x-auto">
              <div class="font-mono text-xs text-zinc-500 dark:text-zinc-400">
                @@ -{hunk.oldStart},{hunk.oldLines} +{hunk.newStart},{hunk.newLines} @@
              </div>
              <For each={rows}>
                {(row, ri) => (
                  <div>
                    <div class="group flex font-mono text-xs">
                      <span class="w-10 shrink-0 select-none text-right text-zinc-400">{row.oldNo ?? ""}</span>
                      <span class="w-10 shrink-0 select-none text-right text-zinc-400">{row.newNo ?? ""}</span>
                      <span class="w-4 shrink-0 select-none">{row.line.t}</span>
                      <span class="whitespace-pre">{row.line.text}</span>
                      <button
                        type="button"
                        class="link ml-2 hidden shrink-0 group-hover:inline"
                        onClick={() => stageLine(props.file, hunk, row)}
                        aria-label={`Comment on line ${row.newNo ?? row.oldNo}`}
                      >
                        +
                      </button>
                    </div>
                    <For each={threadsAt(hi(), ri())}>
                      {(t) => <ThreadCard thread={t} client={props.client} num={props.num} reload={props.reload} canResolve={props.canResolve} />}
                    </For>
                  </div>
                )}
              </For>
            </div>
          );
        }}
      </For>
      {/* Anchors that no longer locate (drifted head, deleted lines)
          render once, collapsed, at the file end — never relocated. */}
      <For each={placement().unplaced}>
        {(t) => <ThreadCard thread={t} client={props.client} num={props.num} reload={props.reload} canResolve={props.canResolve} />}
      </For>
    </div>
  );
}

/** One thread card: comments, resolve toggle, outdated collapse. */
function ThreadCard(props) {
  const t = () => props.thread;
  const [getBody, setBody] = createSignal("");
  const [getOpen, setOpen] = createSignal(!t().resolved);

  const comment = async (e) => {
    e.preventDefault();
    if (!getBody().trim()) return;
    try {
      await props.client.pulls.threads.comment(props.num, t().tid, getBody().trim());
      setBody("");
      props.reload();
    } catch (err) {
      reportError(err, "thread-comment");
    }
  };

  const toggle = async () => {
    try {
      if (t().resolved) await props.client.pulls.threads.unresolve(props.num, t().tid);
      else await props.client.pulls.threads.resolve(props.num, t().tid);
      props.reload();
    } catch (err) {
      reportError(err, "thread-resolve");
    }
  };

  return (
    <div class="ml-14 mt-1 rounded border border-zinc-200 p-2 dark:border-zinc-700" aria-label={`Thread ${t().tid}`}>
      <div class="mb-1 flex flex-wrap items-center gap-2 text-xs">
        <span class="font-mono text-zinc-500 dark:text-zinc-400">{t().tid}</span>
        <Show when={t()._fresh === false}>
          <span class="pill bg-zinc-200 text-zinc-700 dark:bg-zinc-700 dark:text-zinc-300" title="anchor drifted past the current diff">
            outdated · {(t().anchor?.path ?? "")}:{(t().anchor?.side === "NEW" ? t().anchor?.new_start : t().anchor?.old_start) ?? ""}
          </span>
        </Show>
        <Show when={t().resolved}>
          <span class="pill">resolved{t().resolved_by ? ` by ${t().resolved_by}` : ""}</span>
        </Show>
        <Show when={props.canResolve}>
          <button type="button" class="link" onClick={toggle}>
            {t().resolved ? "unresolve" : "resolve"}
          </button>
        </Show>
        <button type="button" class="link" onClick={() => setOpen(!getOpen())}>
          {getOpen() ? "collapse" : "expand"}
        </button>
      </div>
      <Show when={getOpen()}>
        <ThreadComments tid={t().tid} client={props.client} num={props.num} />
        <form class="mt-1 flex gap-2" onSubmit={comment}>
          <input class="input flex-1" value={getBody()} onInput={(e) => setBody(e.target.value)} placeholder="reply…" aria-label="Reply" />
          <button type="submit" class="btn px-2 py-0.5">
            reply
          </button>
        </form>
      </Show>
    </div>
  );
}

/** Lazily loaded comments for one thread card. */
function ThreadComments(props) {
  const key = () => `thread:${props.num}:${props.tid}`;
  const [getView] = useData(key, () => props.client.pulls.threads.get(props.num, props.tid));
  return (
    <ul class="space-y-1">
      <For each={getView()?.comments ?? []} fallback={<li class="text-xs text-zinc-500 dark:text-zinc-400">loading…</li>}>
        {(c) => (
          <li class="text-xs">
            <span class="font-semibold">{c.by}</span> <span class="text-zinc-500 dark:text-zinc-400">{fmtDate(c.at)}</span>
            <p class="whitespace-pre-wrap text-sm">{c.body}</p>
          </li>
        )}
      </For>
    </ul>
  );
}

/** Finish-review modal: pending staged line comments + top-level body +
 *  verdict, one POST reviews (threads open atomically with the review). */
function FinishReview(props) {
  const [getBody, setBody] = createSignal("");
  const [getVerdict, setVerdict] = createSignal("COMMENTED");
  const [getBusy, setBusy] = createSignal(false);

  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      await props.client.pulls.reviews.submit(props.num, {
        state: getVerdict(),
        body: getBody(),
        commit_sha: props.head,
        threads: props.pending,
      });
      props.onDone();
    } catch (err) {
      reportError(err, "review-submit");
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card" aria-label="Finish review" onSubmit={submit}>
      <h2 class="mb-2 text-sm font-semibold">Finish review</h2>
      <Show when={(props.pending ?? []).length > 0} fallback={<p class="text-xs text-zinc-500 dark:text-zinc-400">no staged line comments</p>}>
        <ul class="mb-2 space-y-1">
          <For each={props.pending}>
            {(p, i) => (
              <li class="flex items-start gap-2 text-xs">
                <span class="font-mono">
                  {p.anchor.path}:{(p.anchor.side === "NEW" ? p.anchor.new_start : p.anchor.old_start)}
                </span>
                <span class="flex-1 whitespace-pre-wrap">{p.body}</span>
                <button type="button" class="link" onClick={() => props.onUnstage(i())}>
                  remove
                </button>
              </li>
            )}
          </For>
        </ul>
      </Show>
      <label class="field">
        <span>Body (optional)</span>
        <textarea value={getBody()} onInput={(e) => setBody(e.target.value)} rows="3" />
      </label>
      <label class="field mt-2">
        <span>Verdict</span>
        <select value={getVerdict()} onInput={(e) => setVerdict(e.target.value)}>
          <option value="COMMENTED">comment</option>
          <option value="APPROVED">approve</option>
          <option value="CHANGES_REQUESTED">request changes</option>
        </select>
      </label>
      <p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">reviewing {(props.head ?? "").slice(0, 12)}</p>
      <div class="mt-2 flex gap-2">
        <button type="submit" class="btn btn-primary px-3 py-1" disabled={getBusy()}>
          {getBusy() ? "submitting…" : "submit review"}
        </button>
        <button type="button" class="btn px-3 py-1" onClick={props.onDone}>
          cancel
        </button>
      </div>
    </form>
  );
}

export default function Pull() {
  const ctx = useRepo();
  const params = useParams();
  const num = () => params.num;
  const key = () => `pull:${ctx.full}:${num()}`;
  const reviewsKey = () => `reviews:${ctx.full}:${num()}`;
  const threadsKey = () => `threads:${ctx.full}:${num()}`;
  const requestsKey = () => `requests:${ctx.full}:${num()}`;
  const diffKey = () => `pulldiff:${ctx.full}:${num()}`;
  const [getView] = useData(key, () => ctx.repoClient.pulls.get(num()));
  const [getReviews] = useData(reviewsKey, () => ctx.repoClient.pulls.reviews.list(num(), { n: 50 }));
  const [getThreads] = useData(threadsKey, () => ctx.repoClient.pulls.threads.list(num(), { n: 100 }));
  const [getRequests] = useData(requestsKey, () => ctx.repoClient.pulls.requests.list(num()));
  const [getDiff] = useData(diffKey, async () => {
    const res = await ctx.repoClient.pulls.diff(num());
    const patch = typeof res === "string" ? res : res.patch ?? res.diff ?? "";
    return parsePatchFiles(patch);
  });
  const [getBody, setBody] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getStrategy, setStrategy] = createSignal("merge");
  const [getTask, setTask] = createSignal(null);
  const [getPending, setPending] = createSignal([]);
  const [getFinishing, setFinishing] = createSignal(false);

  const reload = () => {
    invalidate(key());
    invalidate(reviewsKey());
    invalidate(threadsKey());
    invalidate(requestsKey());
    invalidate(diffKey());
  };
  const reloadReview = () => {
    invalidate(key());
    invalidate(reviewsKey());
    invalidate(threadsKey());
    invalidate(requestsKey());
  };

  const thread = () => getView()?.thread;
  const pr = () => getView()?.pr;
  const mergeable = () => getView()?.mergeable;
  const head = () => getView()?.head_live_sha ?? pr()?.head?.sha ?? "";
  const summary = () => thread()?.review_summary;
  const threads = () => getThreads()?.threads ?? [];

  const stage = (draft) => setPending((list) => [...list, draft]);
  const unstage = (i) => setPending((list) => list.filter((_, j) => j !== i));

  const comment = async (e) => {
    e.preventDefault();
    if (!getBody().trim()) return;
    setBusy(true);
    try {
      await ctx.repoClient.pulls.comment(num(), getBody().trim());
      setBody("");
      reload();
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  };

  const pollTask = async () => {
    for (let i = 0; i < 60; i++) {
      try {
        const { task } = await ctx.repoClient.pulls.mergeTask(num());
        setTask(task);
        if (task?.state !== "running") {
          reload();
          return;
        }
      } catch {
        return;
      }
      await new Promise((r) => setTimeout(r, 2000));
    }
  };

  const merge = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      const { task } = await ctx.repoClient.pulls.merge(num(), { strategy: getStrategy() });
      setTask(task);
      pollTask();
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="grid gap-6 lg:grid-cols-[1fr_320px]">
      <section aria-label="Conversation">
        <h1 class="mb-1 text-xl font-semibold">
          #{num()} {thread()?.title}
        </h1>
        <p class="mb-4 text-sm text-zinc-500 dark:text-zinc-400">
          {thread()?.state} · {thread()?.author} · {fmtDate(thread()?.updated_at)}
        </p>
        <div class="mb-4">
          <ReviewSummaryBar summary={summary()} head={head()} />
        </div>
        <ul class="card-list">
          <For each={getView()?.events ?? []} fallback={<li class="card">No events yet.</li>}>
            {(ev) => (
              <li class="card">
                <div class="card-meta">
                  <span>{ev.actor}</span>
                  <span>{fmtDate(ev.at)}</span>
                </div>
                <Show when={eventText(ev)} fallback={<p class="whitespace-pre-wrap">{ev.body}</p>}>
                  <p class="text-sm italic">{eventText(ev)}</p>
                </Show>
              </li>
            )}
          </For>
        </ul>
        <form class="card mt-3" onSubmit={comment}>
          <label class="field">
            <span>Comment</span>
            <textarea value={getBody()} onInput={(e) => setBody(e.target.value)} rows="3" />
          </label>
          <button type="submit" class="btn btn-primary mt-2 px-3 py-1" disabled={getBusy()}>
            {getBusy() ? "posting…" : "comment"}
          </button>
        </form>
        <div class="mt-4 space-y-4">
          <ReviewsList
            num={num()}
            client={ctx.repoClient}
            reviews={getReviews()?.reviews}
            head={head()}
            canDismiss
            reload={reloadReview}
          />
          <Show when={getFinishing()} fallback={
            <button type="button" class="btn px-3 py-1" onClick={() => setFinishing(true)}>
              finish review{(getPending().length ? ` (${getPending().length} staged)` : "")}
            </button>
          }>
            <FinishReview
              num={num()}
              client={ctx.repoClient}
              head={head()}
              pending={getPending()}
              onUnstage={unstage}
              onDone={() => {
                setFinishing(false);
                setPending([]);
                reloadReview();
              }}
            />
          </Show>
          <div aria-label="Files">
            <h2 class="mb-2 text-sm font-semibold">Files ({(getDiff()?.files ?? []).length})</h2>
            <For each={getDiff()?.files ?? []} fallback={<p class="text-sm text-zinc-500 dark:text-zinc-400">loading diff…</p>}>
              {(file) => (
                <div class="mb-4">
                  <DiffFile
                    file={file}
                    files={getDiff()?.files ?? []}
                    threads={threads()}
                    num={num()}
                    client={ctx.repoClient}
                    head={head()}
                    onStage={stage}
                    reload={reloadReview}
                    canResolve
                  />
                </div>
              )}
            </For>
          </div>
        </div>
      </section>
      <aside aria-label="Merge" class="flex flex-col gap-4">
        <div class="card">
          <h2 class="mb-2 text-sm font-semibold">Mergeability</h2>
          <p class="text-sm">{mergeableText(mergeable())}</p>
          <Show when={!getView()?.head_ref_ok}>
            <p class="mt-1 text-xs text-amber-600 dark:text-amber-400">head ref pending — push first.</p>
          </Show>
          <p class="mt-2 text-xs text-zinc-500 dark:text-zinc-400">
            base {pr()?.base?.ref} @ {(pr()?.base?.sha ?? "").slice(0, 12)}
            <br />
            head {pr()?.head?.ref} @ {(pr()?.head?.sha ?? "").slice(0, 12)}
          </p>
          <div class="mt-2 flex gap-2 text-xs">
            <A href={`/${ctx.owner}/${ctx.name}/pull/${num()}/commits`}>commits</A>
            <A href={`/${ctx.owner}/${ctx.name}/pull/${num()}/files`}>files</A>
          </div>
        </div>
        <ReviewersPanel num={num()} client={ctx.repoClient} requested={getRequests()?.reviewers?.map((r) => r.principal)} reload={reloadReview} />
        <Show when={!pr()?.merged && thread()?.state === "open"}>
          <form class="card" onSubmit={merge}>
            <h2 class="mb-2 text-sm font-semibold">Merge</h2>
            <label class="field">
              <span>Strategy</span>
              <select value={getStrategy()} onInput={(e) => setStrategy(e.target.value)}>
                <option value="merge">merge</option>
                <option value="squash">squash</option>
                <option value="rebase">rebase</option>
              </select>
            </label>
            <button type="submit" class="btn btn-primary mt-2 px-3 py-1" disabled={getBusy()}>
              {getBusy() ? "merging…" : "merge pull request"}
            </button>
            <Show when={getTask()}>
              <p class="mt-2 text-xs">
                task {getTask()?.state}
                <Show when={getTask()?.error}>: {getTask()?.error}</Show>
              </p>
              <ul class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
                <For each={getTask()?.progress ?? []}>{(line) => <li>{line}</li>}</For>
              </ul>
            </Show>
          </form>
        </Show>
        <Show when={pr()?.merged}>
          <p class="card text-sm">
            merged as {(pr()?.merge_commit_sha ?? "").slice(0, 12)} by {pr()?.merged_by}
          </p>
        </Show>
      </aside>
    </div>
  );
}
