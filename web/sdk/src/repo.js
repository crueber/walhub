/**
 * Repo client group — created by `client.repo("owner/name")` (12_web_ui.md
 * §1.0/§1.1). Owns the repo surface: refs/tree/blob/raw/commits/commit/
 * resolve/overview + urls deep links + the §1.5 ref stream. Every method
 * accepts trailing `{signal, onProgress, headers}`.
 */
import { ReposError } from "./errors.js";
import { readSse } from "./sse.js";

export class RepoClient {
  /** @param {ReposClient} client @param {string} fullName */
  constructor(client, fullName) {
    this.client = client;
    const name = fullName.replace(/\.git$/, "");
    const slash = name.indexOf("/");
    this.owner = slash === -1 ? name : name.slice(0, slash);
    this.name = slash === -1 ? "" : name.slice(slash + 1);
    this.prefix = `${this.owner}/${this.name}/api`;
  }

  _path(suffix = "") {
    return `/${this.prefix}${suffix}`;
  }

  /**
   * Deep links (§1.1 `repo.urls`): html/clone/api + raw/tree/blob/commit.
   */
  get urls() {
    const b = this.client.base;
    const root = `${b}/${this.owner}/${this.name}`;
    return {
      html: root,
      clone: `${root}.git`,
      api: `${root}/api`,
      raw: (rev, path = "") => `${root}/raw/${rev}${path ? `/${path}` : ""}`,
      tree: (rev, path = "") => `${root}/tree/${rev}${path ? `/${path}` : ""}`,
      blob: (rev, path = "") => `${root}/blob/${rev}${path ? `/${path}` : ""}`,
      commit: (sha) => `${root}/commit/${sha}`,
    };
  }


  /** O(1) head: `GET …/api/refs` → `{head}`. */
  refs(opts) {
    return this.client._call(this._path("/refs"), { method: "GET", ...opts });
  }

  /**
   * Paged ref list, JSON-only accept (§1.1).
   * @param {"branches"|"tags"} kind
   * @param {{q?: string, prefix?: string, after?: string, n?: number}} [query]
   */
  refPage(kind, query = {}, opts) {
    return this.client._call(this._path(`/refs/${kind}${qs(query)}`), {
      method: "GET",
      headers: { Accept: "application/json" },
      sse: false,
      ...opts,
    });
  }

  branches(query, opts) {
    return this.refPage("branches", query, opts);
  }

  tags(query, opts) {
    return this.refPage("tags", query, opts);
  }

  /**
   * SSE ref stream (§1.5, dialect per 07_api.md §7): `event: ref` frames →
   * `onRef({name, sha})` as they arrive; terminal `event: done`. Honors
   * `opts.signal` (the primary cancel path — the UI swaps controllers per
   * keystroke) and returns a cancellation function (§1.6).
   *
   * @param {"branches"|"tags"} kind
   * @param {{q?: string, prefix?: string, after?: string, n?: number}} [query]
   * @param {(ref: {name: string, sha: string}) => void} [onRef]
   * @returns {Promise<() => void>} cancel function; rejects on stream error
   */
  async refStream(kind, query = {}, onRef, opts = {}) {
    const controller = this.client._controller(opts?.signal);
    const req = this.client._request(this._path(`/refs/${kind}${qs(query)}`), {
      headers: { Accept: "text/event-stream" },
      sse: false,
    });
    const run = async () => {
      let res = await this.client._send(req, controller);
      if (res.status === 401 || this.client._isOpaqueRedirect(res)) {
        try {
          await this.client._authenticate();
        } catch (authErr) {
          if (authErr instanceof ReposError) throw authErr;
          throw new ReposError(res.status, "authentication failed", req.url);
        }
        controller.abort();
        const retryController = this.client._controller(opts?.signal);
        res = await this.client._send(req, retryController);
        if (!res.ok) throw new ReposError(res.status, textOrStatus(res), req.url);
        await this._consume(res, onRef, retryController.signal);
        return;
      }
      if (!res.ok) throw new ReposError(res.status, textOrStatus(res), req.url);
      await this._consume(res, onRef, controller.signal);
    };
    try {
      await run();
    } finally {
      // readSse closed the reader (§1.6)
    }
    return () => controller.abort();
  }

  async _consume(res, onRef, signal) {
    await readSse(res, (frame) => {
      if (frame.event === "ref") onRef?.(frame.data);
      // `done` terminates the stream naturally; server closes after it
    }, { signal });
  }

  /** Ref/path split: `GET …/resolve[/{rest}]`. */
  resolve(rest = "", opts) {
    return this.client._call(this._path(`/resolve${rest ? `/${rest}` : ""}`), { method: "GET", ...opts });
  }

  /** Tree at rev/path: `GET …/tree/{rev}[/{path}]`. */
  tree(rev, path = "", opts) {
    return this.client._call(this._path(`/tree/${rev}${path ? `/${path}` : ""}`), { method: "GET", ...opts });
  }

  /** Blob at rev/path: `GET …/blob/{rev}/{path}` (JSON; `contents`/`binary`/`too_large`). */
  blob(rev, path, opts) {
    return this.client._call(this._path(`/blob/${rev}/${path}`), { method: "GET", ...opts });
  }

  /** Raw blob text: `GET …/blob/{rev}/{path}?raw`. */
  raw(rev, path, opts) {
    return this.client._call(this._path(`/blob/${rev}/${path}?raw`), { method: "GET", ...opts });
  }

  /** Commit history: `GET …/commits?ref=&path=&skip=&n=`. */
  commits({ ref, path, skip, n, ...rest } = {}, opts) {
    return this.client._call(this._path(`/commits${qs({ ref, path, skip, n })}`), { method: "GET", ...opts, ...rest });
  }

  /** Commit detail: `GET …/commit/{sha}`. */
  commit(sha, opts) {
    return this.client._call(this._path(`/commit/${sha}`), { method: "GET", ...opts });
  }

  /** WAL health: `GET …/overview`. */
  overview(opts) {
    return this.client._call(this._path("/overview"), { method: "GET", ...opts });
  }

}

function textOrStatus(res) {
  return `HTTP ${res.status}`;
}

/** Query-string helper: skips null/undefined/empty; encodes key and value. */
function qs(params) {
  const parts = [];
  for (const [k, v] of Object.entries(params ?? {})) {
    if (v === undefined || v === null || v === "") continue;
    parts.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
  }
  return parts.length ? `?${parts.join("&")}` : "";
}
