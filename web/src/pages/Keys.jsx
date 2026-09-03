// web/src/pages/Keys.jsx — route "/keys" (17_ssh.md §3): manage your SSH
// public keys. Auth "none" manages the anon principal's keys; logged-in
// principals (oidc email, token principal) manage their own. The registry
// lives in the object store; this page is plain fetch over /api/v1/ssh-keys
// (same dogfood exception as setup — the SDK is repo-scoped).

import { createSignal, onCleanup, For, Show } from "solid-js";
import { A } from "@solidjs/router";

export default function Keys() {
  const [getKeys, setKeys] = createSignal(null);
  const [getError, setError] = createSignal("");
  const [getKey, setKey] = createSignal("");
  const [getTitle, setTitle] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getNotice, setNotice] = createSignal("");
  let debounce = 0;
  onCleanup(() => clearTimeout(debounce));

  const reload = async () => {
    try {
      const res = await fetch("/api/v1/ssh-keys", { credentials: "same-origin" });
      if (!res.ok) {
        setError(`${res.status} ${(await res.text()).slice(0, 200)}`);
        setKeys([]);
        return;
      }
      setKeys(await res.json());
      setError("");
    } catch (e) {
      setError(String(e.message ?? e));
    }
  };
  reload();

  const add = async (e) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const res = await fetch("/api/v1/ssh-keys", {
        method: "POST",
        credentials: "same-origin",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ key: getKey(), title: getTitle() }),
      });
      const body = await res.json().catch(() => ({}));
      if (res.status === 201) {
        setKey("");
        setTitle("");
        setNotice("key added — clone with ssh:// (the key is live immediately)");
        await reload();
      } else {
        setError(`${res.status} ${body.error ?? (await res.text()).slice(0, 200)}`);
      }
    } catch (e2) {
      setError(String(e2.message ?? e2));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id) => {
    setBusy(true);
    try {
      const res = await fetch(`/api/v1/ssh-keys/${encodeURIComponent(id)}`, {
        method: "DELETE",
        credentials: "same-origin",
      });
      if (!res.ok) {
        setError(`${res.status} ${(await res.text()).slice(0, 200)}`);
      } else {
        setNotice("key removed");
        await reload();
      }
    } catch (e) {
      setError(String(e.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const validate = (v) => {
    // mirrors the server's shape check: a type tag, a base64 blob, optional comment
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      const t = v.trim();
      if (t === "") return;
      if (!/^(ssh-(ed25519|dss|rsa)|ecdsa-sha2-\S+|sk-\S+)\s+\S+/.test(t)) {
        setError("not an authorized_keys line: expected \"ssh-ed25519 AAAA... [comment]\"");
      } else if (getError().startsWith("not an authorized_keys")) {
        setError("");
      }
    }, 250);
  };

  return (
    <div class="keys-page space-y-4">
      <h2 class="text-xl font-semibold">SSH keys</h2>
      <p class="muted text-sm">
        Public keys that may clone, fetch and push over{" "}
        <code class="font-mono text-xs">ssh://</code> as <em>you</em> (your principal, your
        access). The transport listener itself is configured on the{" "}
        <A class="text-emerald-700 hover:underline dark:text-emerald-400" href="/setup">
          setup
        </A>{" "}
        page's config.
      </p>

      <Show when={getError()}>
        <div class="rounded-lg border border-red-300 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/60 dark:text-red-200">
          {getError()}
        </div>
      </Show>
      <Show when={getNotice()}>
        <div class="rounded-lg border border-emerald-300 bg-emerald-50 p-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/60 dark:text-emerald-200">
          {getNotice()}
        </div>
      </Show>

      <section class="card p-4">
        <h3 class="mb-2 font-semibold">Add a key</h3>
        <form class="space-y-3" onSubmit={add}>
          <div>
            <label class="mb-1 block text-sm font-medium" for="key-line">
              Public key (authorized_keys line)
            </label>
            <textarea
              id="key-line"
              class="input font-mono text-xs"
              rows="3"
              spellcheck={false}
              placeholder="ssh-ed25519 AAAAC3Nza... ada@laptop"
              value={getKey()}
              onInput={(e) => {
                setKey(e.currentTarget.value);
                validate(e.currentTarget.value);
              }}
            />
          </div>
          <div>
            <label class="mb-1 block text-sm font-medium" for="key-title">
              Title (optional)
            </label>
            <input
              id="key-title"
              class="input md:max-w-md"
              placeholder="work laptop"
              value={getTitle()}
              onInput={(e) => setTitle(e.currentTarget.value)}
            />
          </div>
          <button type="submit" class="btn primary" disabled={getBusy() || getKey().trim() === ""}>
            {getBusy() ? "adding…" : "Add key"}
          </button>
        </form>
      </section>

      <section class="card p-4">
        <h3 class="mb-2 font-semibold">Your keys</h3>
        <Show when={getKeys()} fallback={<p class="muted">loading…</p>}>
          <Show
            when={getKeys().length > 0}
            fallback={<p class="muted">no keys yet — add one above, then clone over ssh://</p>}
          >
            <div class="overflow-x-auto">
              <table class="data-table">
                <thead>
                  <tr>
                    <th>title</th>
                    <th>key</th>
                    <th>added</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  <For each={getKeys()}>
                    {(k) => (
                      <tr>
                        <td>
                          {k.title || <span class="muted">(untitled)</span>}
                        </td>
                        <td>
                          <code class="font-mono text-xs">{shortKey(k.key)}</code>
                        </td>
                        <td class="tabular muted text-xs">
                          {k.created_at ? new Date(k.created_at).toLocaleString() : ""}
                        </td>
                        <td>
                          <button
                            type="button"
                            class="btn px-2 py-1 text-xs"
                            disabled={getBusy()}
                            onClick={() => remove(k.id)}
                          >
                            remove
                          </button>
                        </td>
                      </tr>
                    )}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
        </Show>
      </section>
    </div>
  );
}

// shortKey renders "ssh-ed25519 AAAAC3…comment" without the middle blob.
function shortKey(line) {
  const parts = line.trim().split(/\s+/);
  if (parts.length >= 2 && parts[1].length > 12) {
    return `${parts[0]} ${parts[1].slice(0, 8)}…${parts[1].slice(-4)}${parts[2] ? " " + parts.slice(2).join(" ") : ""}`;
  }
  return line;
}
