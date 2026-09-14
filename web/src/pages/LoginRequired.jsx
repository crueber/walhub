// web/src/pages/LoginRequired.jsx — route "/login-required" (Forgejo #502):
// the "log in to continue" interstitial for anonymous visitors in OIDC
// anonymous-read mode (#345). Every write affordance routes here instead of
// firing its SDK mutation: ?next= names the attempted ACTION url (preserved
// through the OIDC login via /_auth/login?next=) and ?action= names it in
// the copy. Static route registered before /:owner so it never resolves as
// an owner. Solid signals only (D-WEB-6); no new deps.

import { Show } from "solid-js";
import { A, useSearchParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { useData } from "../lib/data.js";
import { sanitizeNextClient, interstitialLoginHref } from "../lib/writeGate.js";

function param(v) {
  return typeof v === "string" && v ? v : "";
}

export default function LoginRequired() {
  const [search] = useSearchParams();
  const next = () => sanitizeNextClient(param(search.next) || "/");
  const action = () => param(search.action) || "this action";
  // Shared discovery cache key (App.jsx fetches it — zero new requests).
  const [getDiscovery] = useData("discovery", () => repos.discovery().catch(() => null));
  const loginHref = () => interstitialLoginHref(getDiscovery(), next());

  const goBack = (e) => {
    e.preventDefault();
    if (typeof window !== "undefined" && window.history.length > 1) window.history.back();
    else window.location.assign(next());
  };

  return (
    <main class="mx-auto w-full max-w-xl px-4 py-10">
      <section class="card p-6" aria-labelledby="login-required-title">
        <h1 id="login-required-title" class="mb-2 text-lg font-semibold">
          Log in to continue
        </h1>
        <p class="mb-4 text-sm">
          <strong>{action()}</strong> needs an account — you are browsing as a guest. Log in and you will land
          right back where you were.
        </p>
        <div class="flex flex-wrap items-center gap-2">
          <Show
            when={loginHref()}
            fallback={<span class="text-sm text-amber-700 dark:text-amber-400">Log in is not enabled on this server.</span>}
          >
            {(href) => (
              <A class="btn primary" href={href()}>
                Log in
              </A>
            )}
          </Show>
          <A class="btn" href={next()}>
            Back
          </A>
          <button type="button" class="btn" onClick={goBack}>
            Cancel
          </button>
        </div>
        <p class="muted mt-4 text-xs">Continuing to: {next()}</p>
      </section>
    </main>
  );
}
