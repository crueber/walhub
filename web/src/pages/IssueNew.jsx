// web/src/pages/IssueNew.jsx — route "/:owner/:name/issues/new" (02 §11):
// the create form (title, markdown-lite body, preview toggle).

import { createSignal, Show } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { reportError } from "../lib/data.js";
import { renderMarkdown } from "../lib/markdown.js";
import { sanitize } from "../lib/sanitize.js";

export default function IssueNew() {
  const ctx = useRepo();
  const navigate = useNavigate();
  const [getTitle, setTitle] = createSignal("");
  const [getBody, setBody] = createSignal("");
  const [getPreview, setPreview] = createSignal(false);
  const [getBusy, setBusy] = createSignal(false);
  const [getError, setError] = createSignal("");

  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const res = await ctx.repoClient.issues.create({ title: getTitle(), body: getBody() });
      navigate(`/${ctx.full}/issues/${res.thread.num}`, { replace: true });
    } catch (err) {
      const msg = String(err?.message ?? err);
      setError(msg);
      reportError(err, "issue-create");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="issue-new mx-auto max-w-2xl">
      <h2 class="mb-3 text-lg font-semibold">New issue</h2>
      <Show when={getError()}>
        <p class="card mb-3 border-red-300 p-3 text-sm text-red-700 dark:border-red-900 dark:text-red-400" role="alert">
          {getError()}
        </p>
      </Show>
      <form class="grid gap-3" onSubmit={submit}>
        <label class="grid gap-1">
          <span class="text-sm font-medium">Title</span>
          <input
            class="input"
            value={getTitle()}
            onInput={(e) => setTitle(e.target.value)}
            maxlength="256"
            required
            placeholder="Publish fails on empty tree"
          />
        </label>
        <label class="grid gap-1">
          <span class="text-sm font-medium">Body</span>
          <Show when={getPreview()} fallback={
            <textarea
              class="input min-h-32 font-mono text-sm"
              value={getBody()}
              onInput={(e) => setBody(e.target.value)}
              placeholder="Steps to reproduce… (markdown-lite; #N links issues)"
            />
          }>
            <div class="card prose-sm p-3" innerHTML={sanitize(renderMarkdown(getBody()))} />
          </Show>
        </label>
        <div class="flex items-center justify-between">
          <button type="submit" class="btn primary" disabled={getBusy()}>
            {getBusy() ? "Creating…" : "Create issue"}
          </button>
          <button
            type="button"
            class="btn px-2 py-0.5 text-xs"
            aria-pressed={getPreview()}
            onClick={() => setPreview((p) => !p)}
          >
            {getPreview() ? "Edit" : "Preview"}
          </button>
        </div>
      </form>
    </div>
  );
}
