/**
 * Admin client group (12_web_ui.md §1.0/§1.1): create/delete, tasks, ops,
 * policy, and settings — attached onto the `RepoClient` returned by
 * `client.repo("owner/name")`. Every method accepts trailing
 * `{signal, onProgress, headers}`.
 */

/**
 * Attach the admin surface (repo root mutations, tasks, ops, policy,
 * settings) onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachAdmin(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);

  /** Repo info: `GET …/api`. */
  repo.get = (opts) => client._call(p(""), { method: "GET", ...opts });
  /** Create (write): `PUT …/api`. */
  repo.create = (opts) => client._call(p(""), { method: "PUT", sse: false, ...opts });
  /** Delete (admin): `DELETE …/api`. */
  repo.delete = (opts) => client._call(p(""), { method: "DELETE", sse: false, ...opts });

  /** Background tasks: `GET …/tasks`. */
  repo.tasks = (opts) => client._call(p("/tasks"), { method: "GET", ...opts });

  /**
   * Attach to a task (§1.1): JSON snapshot by default; with `onEvent`, the SSE
   * attach. Returns `{result, cancel}` when streaming (§1.6).
   */
  repo.task = async (id, onEvent, opts) => {
    if (!onEvent) {
      return client._call(p(`/tasks/${id}`), { method: "GET", ...opts });
    }
    const controller = client._controller(opts?.signal);
    const req = client._request(p(`/tasks/${id}`), { method: "GET" });
    const res = await client._send(req, controller);
    const result = await client._envelope(res, { url: req.url, onProgress: onEvent, signal: controller.signal });
    return { result, cancel: () => controller.abort() };
  };

  /** Op list: `GET …/ops` → {available, recent, bundle_strategies}. */
  repo.opsList = (opts) => client._call(p("/ops"), { method: "GET", ...opts });

  /**
   * Run an op: `POST …/ops/{op}` → SSE attach (07_api.md §10). `onEvent` gets
   * notice/progress/task frames; the terminal `result` resolves. Returns
   * `{result, cancel}` (§1.6).
   */
  repo.opRun = async (op, params, onEvent, opts = {}) => {
    const controller = client._controller(opts.signal);
    const req = client._request(p(`/ops/${encodeURIComponent(op)}`), {
      method: "POST",
      body: JSON.stringify(params ?? {}),
      headers: { "Content-Type": "application/json" },
      sse: true,
    });
    const res = await client._send(req, controller);
    const result = await client._envelope(res, { url: req.url, onProgress: onEvent, signal: controller.signal });
    return { result, cancel: () => controller.abort() };
  };

  /** `ops.list()` / `ops.run(op, params, onEvent)` per the §1.1 table naming. */
  repo.ops = {
    list: () => repo.opsList(),
    run: (op, params, onEvent, opts) => repo.opRun(op, params, onEvent, opts),
  };

  /** Policy surface (§1.1): get/put/delete/validate/dryRun. */
  repo.policy = {
    get: (opts) => client._call(p("/policy"), { method: "GET", ...opts }),
    put: (policy, opts) =>
      client._call(p("/policy"), {
        method: "PUT",
        body: JSON.stringify(policy),
        headers: { "Content-Type": "application/json" },
        sse: false,
        ...opts,
      }),
    delete: (opts) => client._call(p("/policy"), { method: "DELETE", sse: false, ...opts }),
    validate: (policy, opts) =>
      client._call(p("/policy/validate"), {
        method: "POST",
        body: JSON.stringify(policy),
        headers: { "Content-Type": "application/json" },
        sse: false,
        ...opts,
      }),
    dryRun: (last, opts) =>
      client._call(p(`/policy/dry-run${last ? `?last=${last}` : ""}`), { method: "POST", sse: false, ...opts }),
  };

  /** Settings surface (§1.1): get/put/delete/effective/history/describe/validate. */
  repo.settings = {
    get: (opts) => client._call(p("/settings"), { method: "GET", ...opts }),
    put: (toml, message, opts) =>
      client._call(p(`/settings${message ? `?message=${encodeURIComponent(message)}` : ""}`), {
        method: "PUT",
        body: toml,
        headers: { "Content-Type": "application/toml" },
        sse: false,
        ...opts,
      }),
    delete: (opts) => client._call(p("/settings"), { method: "DELETE", sse: false, ...opts }),
    effective: (opts) => client._call(p("/settings/effective"), { method: "GET", sse: false, ...opts }),
    history: (opts) => client._call(p("/settings/history"), { method: "GET", ...opts }),
    describe: (opts) => client._call(p("/settings/describe"), { method: "GET", ...opts }),
    validate: (toml, opts) =>
      client._call(p("/settings/validate"), {
        method: "POST",
        body: toml,
        headers: { "Content-Type": "application/toml" },
        sse: false,
        ...opts,
      }),
  };
}
