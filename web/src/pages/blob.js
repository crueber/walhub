// web/src/pages/blob.js — Code tab blob view (§2.7 decision tree):
// too_large → placeholder; binary → "binary file, N bytes"; .md → Preview|Code
// toggle (markdown-lite + sanitizer in preview, raw text in code view); else
// line-numbered <pre> tinted by the mini tokenizer.

import { createRoot, createEffect, createSignal, el } from "lib/reactive.js";
import { useResolved } from "lib/data.js";
import { renderMarkdown } from "lib/markdown.js";
import { sanitize } from "lib/sanitize.js";
import { languageFor, highlight } from "lib/highlight.js";
import { shortRef } from "./repo.js";

export function mount(container, ctx) {
  return createRoot((dispose) => {
    const { full } = ctx;
    const rest = ctx.rest ?? "";
    const [getBlob] = useResolved(ctx.owner, ctx.name, rest, "blob");
    const [getView, setView] = createSignal("preview");
    const root = el("div", { class: "blob-page" });
    container.append(root);

    createEffect(() => {
      const b = getBlob();
      if (!b) {
        root.replaceChildren(el("p", { class: "muted" }, "loading blob…"));
        return;
      }
      const name = b.name ?? (b.path ?? "").split("/").pop() ?? "";
      const parts = (b.path ?? "").split("/").filter(Boolean);
      const crumbParts = [el("a", { href: `/${full}` }, shortRef(b.ref))];
      parts.forEach((part, i) => {
        crumbParts.push(" / ");
        crumbParts.push(i === parts.length - 1
          ? el("strong", {}, part)
          : el("a", { href: `/${full}/tree/${shortRef(b.ref)}/${parts.slice(0, i + 1).join("/")}` }, part));
      });
      const crumb = el("nav", { class: "crumbs" }, ...crumbParts);

      const head = el("div", { class: "blob-head" },
        el("h2", {}, name),
        el("span", { class: "muted" }, `${b.size ?? "?"} bytes · ${String(b.sha).slice(0, 12)}`));

      const body = el("div", { class: "blob-body card" });

      if (b.too_large) {
        body.append(el("p", { class: "placeholder" }, `This file is too large to render (${b.size ?? "?"} bytes; the render cap is 2 MiB). Fetch it raw from the API.`));
      } else if (b.binary) {
        body.append(el("p", { class: "placeholder" }, `binary file, ${b.size ?? "?"} bytes`));
      } else if (/\.(md|markdown)$/i.test(name)) {
        const toggle = el("div", { class: "seg" },
          el("button", { type: "button", class: "pill", onclick: () => setView("preview") }, "Preview"),
          el("button", { type: "button", class: "pill", onclick: () => setView("code") }, "Code"));
        head.append(toggle);
        const preview = el("div", { class: "markdown-body" });
        const codeView = el("pre", { class: "blob-pre" }, el("code", {}));
        createEffect(() => {
          const v = getView();
          const text = b.contents ?? "";
          if (v === "preview") {
            codeView.hidden = true;
            preview.hidden = false;
            // sanitizer is the only innerHTML gate
            preview.innerHTML = sanitize(renderMarkdown(text));
          } else {
            preview.hidden = true;
            codeView.hidden = false;
            codeView.firstChild.innerHTML = highlight(text, languageFor(name));
          }
        });
        body.append(preview, codeView);
      } else {
        const text = b.contents ?? "";
        const lines = text.split("\n");
        if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();
        const gutter = el("pre", { class: "blob-gutter tabular" }, lines.map((_, i) => `${i + 1}\n`).join(""));
        const codePre = el("pre", { class: "blob-pre" }, el("code", {}));
        codePre.firstChild.innerHTML = highlight(text, languageFor(name));
        body.append(el("div", { class: "blob-cols" }, gutter, codePre));
      }

      root.replaceChildren(crumb, head, body);
    });
    return dispose;
  });
}
