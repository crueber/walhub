// web/src/pages/Landing.jsx — route "/": the landing page (issue #187).
// Static marketing, ZERO API calls (no useData, no SDK import, no fetch):
// the front door spends no store round trips. Works in both themes (dark
// default); concept GIFs ship in dark-framed cards in both (README
// screenshot precedent — one asset set, no light variants).

import { A } from "@solidjs/router";
import ConceptGif from "../components/ConceptGif.jsx";

export const LANDING_CTAS = [
  { label: "Browse repositories", href: "/explore", primary: true },
  { label: "Push in 30 seconds", href: "#quickstart", primary: false },
  { label: "Configure", href: "/setup", primary: false },
];

export const CONCEPT_ALTS = {
  push: "Animation: a laptop pushes a pack of git objects to a bucket; the bucket writes them, then acknowledges. The client only finishes after the bucket's acknowledgement.",
  bucket:
    "Animation: one bucket holds refs, packs, config, and policy while a server instance above it disappears and a fresh instance connects to the same bucket with nothing lost.",
  fetch:
    "Animation: an empty clone first receives ref names, then pack data, from the bucket until it holds a working copy.",
  collab:
    "Animation: alongside the git lane, issue 7 opens, pull request 8 links to it, a green check result stamps the PR — while the write-ahead log stays git-only.",
};

function ConceptSection(props) {
  return (
    <section class="grid grid-cols-1 items-center gap-6 py-8 md:grid-cols-2">
      <div class={props.flip ? "md:order-2" : ""}>
        <ConceptGif name={props.name} alt={props.alt} caption={props.caption} />
      </div>
      <div class={props.flip ? "md:order-1" : ""}>
        <h3 class="text-lg font-semibold tracking-tight">{props.title}</h3>
        <p class="muted mt-2 text-sm leading-relaxed">{props.children}</p>
      </div>
    </section>
  );
}

export default function Landing() {
  return (
    <div class="landing-page mx-auto max-w-4xl">
      <section class="py-10 text-center">
        <h1 class="text-3xl font-bold tracking-tight">
          walhub — a git host whose only database is an object store.
        </h1>
        <p class="muted mx-auto mt-4 max-w-2xl text-sm leading-relaxed">
          It serves repositories whose entire state — refs, packs, config, policy, events, web UI —
          lives as objects in a bucket (filesystem, S3, or GCS). Instances are disposable; wipe one
          and you lose nothing but warmth.
        </p>
        <p class="muted mt-4 flex flex-wrap items-center justify-center gap-3">
          <A class="btn primary px-4 py-2" href="/explore">
            Browse repositories
          </A>
          <a class="btn px-4 py-2" href="#quickstart">
            Push in 30 seconds
          </a>
        </p>
        <p class="muted mt-3 text-sm">
          <A class="hover:underline" href="/setup">
            Configure
          </A>
        </p>
        <p class="muted mt-4 text-xs">
          Object-protocol compliant with walgit — bucket layout, protobuf wire encoding, and git wire
          behavior follow walgit's formats.
        </p>
        <p class="muted mt-4 flex flex-wrap items-center justify-center gap-3 text-sm">
          <A class="btn px-4 py-2" href="/how-it-works">
            How it works →
          </A>
          <span>the object-store idea and the WAL, in depth.</span>
        </p>
      </section>

      <div class="divide-y divide-zinc-200 dark:divide-zinc-800">
        <ConceptSection
          name="push"
          alt={CONCEPT_ALTS.push}
          caption="Push over smart HTTP; objects land in the bucket."
          title="Push"
        >
          Push over smart HTTP (v0/v2) with repositories auto-created on push. The manifest CAS is
          the only commit point — the server never acknowledges your push before the bucket does.
        </ConceptSection>
        <ConceptSection
          name="bucket"
          alt={CONCEPT_ALTS.bucket}
          caption="No SQL, no Redis. Kill the instance; the bucket doesn't notice."
          title="The bucket is the database"
          flip
        >
          Every repository's refs, packs, config, and policy live as objects in the bucket. Disk and
          memory are caches: a fresh instance serves the same repositories immediately.
        </ConceptSection>
        <ConceptSection
          name="fetch"
          alt={CONCEPT_ALTS.fetch}
          caption="Clone reads objects back."
          title="Fetch / clone"
          flip={false}
        >
          Warm refs resolve in one round trip through the stock-git fetch path; bundle-uri carries
          large hosts. What the bucket stored, your clone receives.
        </ConceptSection>
        <ConceptSection
          name="collab"
          alt={CONCEPT_ALTS.collab}
          caption="Issues, PRs, reviews, and checks live next to git data."
          title="Collaboration as objects"
          flip
        >
          Issues, pull requests, reviews, and checks live in a parallel object family next to git
          data — with one architectural law: the write-ahead log stays git-only and never gates a
          push.
        </ConceptSection>
      </div>

      <section id="quickstart" class="card my-8 scroll-mt-16 p-4">
        <h2 class="text-lg font-semibold">Push in 30 seconds</h2>
        <pre class="mt-3 overflow-x-auto rounded bg-zinc-100 p-3 text-xs leading-relaxed dark:bg-zinc-900">
          {`make build          # bundles the SDK (esbuild) and compiles the binary — web/ is embedded
./walhub            # zero-config: 0.0.0.0:8080, filesystem store, auth "none" (loud warning)

mkdir demo && cd demo && git init -b main
echo "# demo" > README.md && git add -A && git commit -m "hi"
git remote add origin http://localhost:8080/you/demo.git
git push -u origin main          # browse it at http://localhost:8080/you/demo`}
        </pre>
        <p class="muted mt-3 text-sm">
          Auth: <code>none</code> for dev, <code>token</code> for static bearers, <code>oidc</code>{" "}
          for real users — then <A class="hover:underline" href="/setup">/setup</A>. Git also works
          over SSH on port 2222 — add your public key on the{" "}
          <A class="hover:underline" href="/keys">/keys</A> page.
        </p>
        <p class="mt-4 flex flex-wrap items-center gap-3">
          <A class="btn primary px-4 py-2" href="/explore">
            Browse repositories →
          </A>
          <A class="btn px-4 py-2" href="/how-it-works">
            How it works →
          </A>
        </p>
        <p class="muted mt-3 text-sm">
          New here? The object-store idea and the WAL, in depth.
        </p>
      </section>
    </div>
  );
}
