// web/src/lib/icons.jsx — the shared embedded SVG icon mechanism (Forgejo
// #465): one component, consumed by all six controls (Watch/Star/Fork/Clone
// in pages/Repo.jsx, the notification bell in components/NotificationTray.jsx,
// the theme toggle in App.jsx).
//
// The 10 icon bodies are embedded below as inline JSX, transcribed verbatim
// from the provided 1em currentColor files (issue comments 4783-4792 — the
// source of truth for every path; each entry keeps its file's viewBox as-is
// so the mixed viewports 16/24/1024/1200 all scale through width="1em"
// height="1em"). No fetches, no raw imports, no innerHTML: the icons ship
// inside the vite bundle and render with zero runtime requests. No color
// literals anywhere in this layer — every body paints via fill="currentColor"
// / stroke="currentColor" and state coloring comes from the control's own
// classes (the primary class on toggles stays the other half of the state
// signal, doubled with the icon swap so state is never color-only).
//
// Size/spacing belongs to the caller: the Icon component always carries the
// one shared .icon utility (web/src/ui.css: inline-block, 1em box, no shrink,
// baseline-aligned for non-flex contexts) plus any extra class the caller
// passes. The header pills need nothing more — .btn is already inline-flex
// with gap, so icon and label align identically at text-sm and at default
// sizes across the mixed viewBoxes.

import { Show } from "solid-js";

// Viewport per icon, exactly as shipped in the provided files (mixed units
// are the point: the 1em width/height on the shared <svg> below scales every
// one of these to the caller's font size).
const ICONS = {
  "watch-on": {
    viewBox: "0 0 16 16",
    body: (
      <path fill="currentColor" d="M8 3c3.218 0 5.788 3.235 6.67 4.501a.865.865 0 0 1 0 .998C13.789 9.765 11.219 13 8 13S2.21 9.765 1.329 8.499a.865.865 0 0 1 0-.998c.882-1.266 3.453-4.5 6.67-4.501m0 2a3 3 0 1 0 0 6a3 3 0 0 0 0-6" />
    ),
  },
  "watch-off": {
    viewBox: "0 0 16 16",
    body: (
      <path fill="currentColor" d="M13.21 6.386a1 1 0 0 1 1.687 1.054l-.05.09v.003l-.003.004l-.01.013l-.027.043l-.097.145c-.084.123-.207.294-.363.497c-.205.265-.473.586-.794.931a1 1 0 0 1 .279.28l1 1.5a1 1 0 1 1-1.664 1.109l-1-1.5a1 1 0 0 1-.05-.083c-.85.643-1.905 1.22-3.118 1.436V14a1 1 0 0 1-2 0v-2.092c-1.213-.215-2.269-.793-3.12-1.436q-.021.042-.048.083l-1 1.5a1 1 0 0 1-1.664-1.11l1-1.5a1 1 0 0 1 .278-.279a13 13 0 0 1-.793-.93a11 11 0 0 1-.46-.643l-.028-.043l-.009-.013l-.003-.004v-.002A1 1 0 1 1 2.847 6.47l.002.004l.016.024l.072.108a11 11 0 0 0 1.435 1.666C5.366 9.206 6.629 10 8 10s2.634-.794 3.627-1.729a11 11 0 0 0 1.434-1.666q.05-.072.073-.108l.016-.024l.002-.004z" />
    ),
  },
  "star-on": {
    viewBox: "0 0 24 24",
    body: (
      <g fill="none" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round" stroke-width="2">
        <path d="M9 8c-1.667.667-5.4 2.7-7 5.5m9.5-2.5C9.167 12.333 4 16.4 2 22m10.5-7.5c-1.167 1.167-3.8 4.1-5 6.5" />
        <path fill="currentColor" d="m14.674 6.45l.673-3.285l2.225 2.51l3.027-.294l-1.768 3.062l1.743 2.639l-3.286-.673l-2.51 2.225l.19-3.156l-3.062-1.768z" />
      </g>
    ),
  },
  "star-off": {
    viewBox: "0 0 24 24",
    body: (
      <path fill="none" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 8c-1.667.667-5.4 2.7-7 5.5m9.5-2.5C9.167 12.333 4 16.4 2 22m10.5-7.5c-1.167 1.167-3.8 4.1-5 6.5m7.174-14.55l.673-3.285l2.225 2.51l3.027-.294l-1.768 3.062l1.743 2.639l-3.286-.673l-2.51 2.225l.19-3.156l-3.062-1.768z" />
    ),
  },
  fork: {
    viewBox: "0 0 1200 1200",
    body: (
      <path fill="currentColor" d="M42.203 935.926h186.061V763.958c0-54.408 26.559-114.484 77.32-164.729c50.762-50.242 126.065-104.842 249.904-191.527c124.394-87.076 199.565-135.567 233.346-165.807c33.78-30.24 30.882-25.376 30.882-69.388V0h147.863v172.507c0 66.078-27.619 132.54-80.093 179.516s-125.164 91.312-247.208 176.741c-122.601 85.82-195.159 140.381-230.651 175.512c-35.491 35.129-33.5 36.641-33.5 59.685v171.967h194.147L306.276 1200zm587.524 0h189.988V763.958c0-23.043 1.914-24.554-33.577-59.684c-23.477-23.237-65.093-56.146-124.76-99.809c7.49-5.281 13.418-9.555 21.333-15.095c43.674-30.571 75.183-51.648 107.816-73.777c41.578 31.395 73.875 58.12 99.652 83.637c50.763 50.242 77.397 110.319 77.397 164.729v171.968h190.22L893.801 1200z" />
    ),
  },
  clone: {
    viewBox: "0 0 1024 1024",
    body: (
      <>
        <path fill="currentColor" d="M624 706.3h-74.1V464c0-4.4-3.6-8-8-8h-60c-4.4 0-8 3.6-8 8v242.3H400c-6.7 0-10.4 7.7-6.3 12.9l112 141.7a8 8 0 0 0 12.6 0l112-141.7c4.1-5.2.4-12.9-6.3-12.9" />
        <path fill="currentColor" d="M811.4 366.7C765.6 245.9 648.9 160 512.2 160S258.8 245.8 213 366.6C127.3 389.1 64 467.2 64 560c0 110.5 89.5 200 199.9 200H304c4.4 0 8-3.6 8-8v-60c0-4.4-3.6-8-8-8h-40.1c-33.7 0-65.4-13.4-89-37.7c-23.5-24.2-36-56.8-34.9-90.6c.9-26.4 9.9-51.2 26.2-72.1c16.7-21.3 40.1-36.8 66.1-43.7l37.9-9.9l13.9-36.6c8.6-22.8 20.6-44.1 35.7-63.4a245.6 245.6 0 0 1 52.4-49.9c41.1-28.9 89.5-44.2 140-44.2s98.9 15.3 140 44.2c19.9 14 37.5 30.8 52.4 49.9c15.1 19.3 27.1 40.7 35.7 63.4l13.8 36.5l37.8 10C846.1 454.5 884 503.8 884 560c0 33.1-12.9 64.3-36.3 87.7a123.07 123.07 0 0 1-87.6 36.3H720c-4.4 0-8 3.6-8 8v60c0 4.4 3.6 8 8 8h40.1C870.5 760 960 670.5 960 560c0-92.7-63.1-170.7-148.6-193.3" />
      </>
    ),
  },
  "notify-on": {
    viewBox: "0 0 1024 1024",
    body: (
      <path fill="currentColor" d="M880 112c-3.8 0-7.7.7-11.6 2.3L292 345.9H128c-8.8 0-16 7.4-16 16.6v299c0 9.2 7.2 16.6 16 16.6h101.6c-3.7 11.6-5.6 23.9-5.6 36.4c0 65.9 53.8 119.5 120 119.5c55.4 0 102.1-37.6 115.9-88.4l408.6 164.2c3.9 1.5 7.8 2.3 11.6 2.3c16.9 0 32-14.2 32-33.2V145.2C912 126.2 897 112 880 112M344 762.3c-26.5 0-48-21.4-48-47.8c0-11.2 3.9-21.9 11-30.4l84.9 34.1c-2 24.6-22.7 44.1-47.9 44.1" />
    ),
  },
  "notify-off": {
    viewBox: "0 0 1024 1024",
    body: (
      <path fill="currentColor" d="M880 112c-3.8 0-7.7.7-11.6 2.3L292 345.9H128c-8.8 0-16 7.4-16 16.6v299c0 9.2 7.2 16.6 16 16.6h101.7c-3.7 11.6-5.7 23.9-5.7 36.4c0 65.9 53.8 119.5 120 119.5c55.4 0 102.1-37.6 115.9-88.4l408.6 164.2c3.9 1.5 7.8 2.3 11.6 2.3c16.9 0 32-14.2 32-33.2V145.2C912 126.2 897 112 880 112M344 762.3c-26.5 0-48-21.4-48-47.8c0-11.2 3.9-21.9 11-30.4l84.9 34.1c-2 24.6-22.7 44.1-47.9 44.1m496 58.4L318.8 611.3l-12.9-5.2H184V417.9h121.9l12.9-5.2L840 203.3z" />
    ),
  },
  "light-mode": {
    viewBox: "0 0 1024 1024",
    body: (
      <path fill="currentColor" fill-rule="evenodd" d="M548 818v126c0 8.837-7.163 16-16 16h-40c-8.837 0-16-7.163-16-16V818q23.768 2.464 36 2.464T548 818m205.251-115.66l89.096 89.095c6.248 6.248 6.248 16.38 0 22.627l-28.285 28.285c-6.248 6.248-16.379 6.248-22.627 0L702.34 753.25q18.548-15.064 27.198-23.713q8.649-8.65 23.713-27.198m-482.502 0q15.064 18.548 23.713 27.198q8.65 8.649 27.198 23.713l-89.095 89.096c-6.248 6.248-16.38 6.248-22.627 0l-28.285-28.285c-6.248-6.248-6.248-16.379 0-22.627zM512 278c129.235 0 234 104.765 234 234S641.235 746 512 746S278 641.235 278 512s104.765-234 234-234M206 476q-2.464 23.768-2.464 36T206 548H80c-8.837 0-16-7.163-16-16v-40c0-8.837 7.163-16 16-16zm738 0c8.837 0 16 7.163 16 16v40c0 8.837 7.163 16 16 16H818q2.464-23.768 2.464-36T818 476ZM814.062 180.653l28.285 28.285c6.248 6.248 6.248 16.379 0 22.627L753.25 320.66q-15.064-18.548-23.713-27.198q-8.65-8.649-27.198-23.713l89.095-89.096c6.248-6.248 16.38-6.248 22.627 0m-581.497 0l89.095 89.096q-18.548 15.064-27.198 23.713q-8.649 8.65-23.713 27.198l-89.096-89.095c-6.248-6.248-6.248-16.38 0-22.627l28.285-28.285c6.248-6.248 16.379-6.248 22.627 0M532 64c8.837 0 16 7.163 16 16v126q-23.768-2.464-36-2.464T476 206V80c0-8.837 7.163-16 16-16z" />
    ),
  },
  "dark-mode": {
    viewBox: "0 0 24 24",
    body: (
      <g fill="none" stroke="currentColor" stroke-linecap="round" stroke-width="2">
        <path d="M12 3V2m0 20v-1m9-9h1M2 12h1m15.5-6.5L20 4M4 20l1.5-1.5M4 4l1.5 1.5m13 13L20 20" />
        <circle cx="12" cy="12" r="4" />
      </g>
    ),
  },
};

/** The ten icon names, in asset order (watch, star, fork, clone, bell, theme). */
export const ICON_NAMES = Object.keys(ICONS);

/**
 * Render one embedded icon inline: `<Icon name="watch-on" />`.
 * The icon is decorative — the svg always carries aria-hidden="true" and the
 * control's own title/aria-label/aria-pressed stays the accessible signal.
 * Unknown names render nothing (never a broken glyph).
 */
export default function Icon(props) {
  const entry = () => ICONS[props.name];
  return (
    <Show when={entry()}>
      {(e) => (
        <svg
          xmlns="http://www.w3.org/2000/svg"
          width="1em"
          height="1em"
          viewBox={e().viewBox}
          aria-hidden="true"
          class={props.class ? `icon ${props.class}` : "icon"}
        >
          {e().body}
        </svg>
      )}
    </Show>
  );
}
