// web/src/components/ConceptGif.jsx — landing-page concept figure (issue #187).
// Still-first loading: the `-still` frame renders immediately (and is the
// <noscript> output); JS swaps in the animated .gif only when the user has
// NOT asked for reduced motion. One accessible name per figure: plain
// `img alt` (never a labeled figure wrapper on top — N1 double-announce).

import { createSignal, onCleanup, onMount } from "solid-js";

/** Resolve the animated source for a concept name (served prefix pinned: /_ui/concepts/ — B1). */
export function conceptSrc(name) {
  return `/_ui/concepts/${name}.gif`;
}

/** Resolve the still source for a concept name (reduced-motion + noscript). */
export function conceptStillSrc(name) {
  return `/_ui/concepts/${name}-still.gif`;
}

export default function ConceptGif(props) {
  const still = () => conceptStillSrc(props.name);
  const animated = () => conceptSrc(props.name);
  const [src, setSrc] = createSignal(still());

  let mq = null;
  const apply = () => {
    const reduce = mq != null && mq.matches === true;
    setSrc(reduce ? still() : animated());
  };
  const onChange = () => apply();

  onMount(() => {
    if (typeof window !== "undefined" && typeof window.matchMedia === "function") {
      mq = window.matchMedia("(prefers-reduced-motion: reduce)");
      apply();
      if (typeof mq.addEventListener === "function") mq.addEventListener("change", onChange);
      else if (typeof mq.addListener === "function") mq.addListener(onChange);
    } else {
      // No matchMedia (SSR/tests): still stays — safe default.
    }
  });
  onCleanup(() => {
    if (mq != null) {
      if (typeof mq.removeEventListener === "function") mq.removeEventListener("change", onChange);
      else if (typeof mq.removeListener === "function") mq.removeListener(onChange);
    }
  });

  return (
    <figure class="concept-figure overflow-hidden rounded-xl border border-zinc-800 bg-[#09090b]">
      <img
        src={src()}
        alt={props.alt}
        width="640"
        height="360"
        loading="lazy"
        decoding="async"
      />
      <noscript>
        <img
          src={still()}
          alt={props.alt}
          width="640"
          height="360"
          loading="lazy"
          decoding="async"
        />
      </noscript>
      <figcaption class="concept-caption muted px-4 py-2 text-sm">{props.caption}</figcaption>
    </figure>
  );
}
