/**
 * attachUpload.js — shared paste/drop image-upload logic for issue
 * composers (docs/features/02 §12). Pure string/byte helpers, DOM-thin:
 * pages own their textareas and call these; importable in Node (no DOM).
 *
 * Flow per file: `placeholderFor(name)` is spliced at the cursor via
 * `insertAtCursor`, the SDK uploads the bytes, then `replacePlaceholder`
 * swaps the placeholder for `markdownFor(name, url)`. Failures swap in an
 * `uploadFailedFor(name, detail)` line instead (the caller also
 * `reportError`s verbatim per the plain-text error contract). Uploads run
 * SEQUENTIALLY per composer (no unbounded fan-out; each is its own
 * request). Bodies stay ≤ 64 KiB: `projectedLengthOk` pre-checks before
 * each upload so a body pushed over the cap 400s client-side instead of
 * orphaning bytes server-side.
 */

/** Raw-text body cap shared with the server validateBody (02 §1.2). */
export const MAX_BODY_BYTES = 64 * 1024;

/** Fallback image extensions when the clipboard item carries no MIME type. */
const IMAGE_EXTS = new Set(["png", "jpg", "jpeg", "gif", "webp"]);

/**
 * True when a pasted/dropped file is an acceptable image: MIME image/*
 * except SVG (scriptable XML — the server 415s it, so never upload it),
 * or a bare file with an image extension when the type is empty.
 * @param {{type?: string, name?: string}} file
 */
export function isImageFile(file) {
  const type = String(file?.type ?? "").toLowerCase();
  if (type === "image/svg+xml") return false;
  if (type.startsWith("image/")) return true;
  if (!type) {
    const m = /\.([a-z0-9]+)$/i.exec(String(file?.name ?? ""));
    if (m && IMAGE_EXTS.has(m[1].toLowerCase())) return true;
  }
  return false;
}

/**
 * Escape a filename for the `![alt]` slot (S1): backslash first, then
 * the closers that would terminate the alt or the URL early. The
 * markdown-lite renderer keeps backslashes literal in alt text — safe,
 * slightly verbose, never injectable.
 */
export function escapeAlt(name) {
  return String(name ?? "")
    .replace(/\\/g, "\\\\")
    .replace(/\]/g, "\\]")
    .replace(/\[/g, "\\[")
    .replace(/\(/g, "\\(")
    .replace(/\)/g, "\\)");
}

/** Inline uploading marker spliced at the cursor while one file uploads. */
export function placeholderFor(name) {
  return `![uploading ${name}…]()`;
}

/** Final markdown once the server answers 201 (url arrives percent-encoded). */
export function markdownFor(name, url) {
  return `![${escapeAlt(name)}](${url})`;
}

/** Failure line replacing the placeholder (caller also reportError()s). */
export function uploadFailedFor(name, detail) {
  const extra = detail ? `: ${String(detail).split("\n")[0].slice(0, 160)}` : "";
  return `![upload of ${name} failed${extra}]()`;
}

/**
 * Splice `insert` over [start, end) of text (selection or caret);
 * out-of-range indices clamp. Returns the new text + cursor (end of insert).
 */
export function insertAtCursor(text, start, end, insert) {
  const src = String(text ?? "");
  const ins = String(insert ?? "");
  let s = Math.max(0, Math.min(Number(start), src.length));
  let e = Math.max(0, Math.min(Number(end), src.length));
  if (Number.isNaN(s)) s = src.length;
  if (Number.isNaN(e)) e = s;
  if (e < s) [s, e] = [e, s];
  return { text: src.slice(0, s) + ins + src.slice(e), cursor: s + ins.length };
}

/**
 * Swap the first `placeholder` occurrence for `replacement`; when the user
 * edited the placeholder away meanwhile, append instead (never lose the URL).
 */
export function replacePlaceholder(text, placeholder, replacement) {
  const src = String(text ?? "");
  const at = src.indexOf(placeholder);
  if (at < 0) return src === "" ? replacement : `${src}\n${replacement}`;
  return src.slice(0, at) + replacement + src.slice(at + placeholder.length);
}

/** True when body + pending additions still fit the 64 KiB raw-text cap. */
export function projectedLengthOk(body, additions, limit = MAX_BODY_BYTES) {
  return String(body ?? "").length + String(additions ?? "").length <= limit;
}

/**
 * Image files carried by a paste event (clipboardData.files first,
 * clipboardData.items fallback for screenshots that expose no FileList).
 * Returns [] for text pastes — the caller must NOT preventDefault then.
 */
export function filesFromPasteEvent(e) {
  const out = [];
  const files = e?.clipboardData?.files;
  if (files?.length) {
    for (const f of files) if (isImageFile(f)) out.push(f);
    if (out.length) return out;
  }
  const items = e?.clipboardData?.items;
  if (items?.length) {
    for (const it of items) {
      if (it?.kind !== "file") continue;
      const f = it.getAsFile?.();
      if (f && isImageFile(f)) out.push(f);
    }
  }
  return out;
}

/** Image files carried by a drop event (image-kind only, like paste). */
export function filesFromDropEvent(e) {
  const out = [];
  const files = e?.dataTransfer?.files;
  if (files?.length) {
    for (const f of files) if (isImageFile(f)) out.push(f);
  }
  return out;
}

/**
 * Sequentially upload files into a composer: placeholder at the cursor →
 * 201 markdown (or a failure line + onError). Skips a file whose
 * placeholder would push the body over the 64 KiB cap (no orphan bytes).
 * Operates on getText/setText callbacks + an optional textarea element
 * for cursor placement; Solid-agnostic so IssueNew and CommentComposer
 * share it.
 */
export async function uploadFilesSequential({ files, textarea, getText, setText, upload, onError }) {
  for (const file of files ?? []) {
    if (!isImageFile(file)) continue;
    const name = file?.name || "image";
    const placeholder = placeholderFor(name);
    const current = String(getText?.() ?? "");
    if (!projectedLengthOk(current, placeholder)) {
      onError?.(new Error(`image ${name} would push the body over 64 KiB`));
      continue;
    }
    const start = Number(textarea?.selectionStart ?? current.length);
    const end = Number(textarea?.selectionEnd ?? start);
    const ins = insertAtCursor(current, start, end, placeholder);
    setText?.(ins.text);
    try {
      textarea?.setSelectionRange?.(ins.cursor, ins.cursor);
    } catch {
      /* headless/off-DOM */
    }
    try {
      const rec = await upload(file);
      setText?.(replacePlaceholder(String(getText?.() ?? ""), placeholder, markdownFor(rec?.name ?? name, rec?.url)));
    } catch (err) {
      setText?.(replacePlaceholder(String(getText?.() ?? ""), placeholder, uploadFailedFor(name, err?.message)));
      onError?.(err);
    }
  }
  try {
    textarea?.focus?.();
  } catch {
    /* headless/off-DOM */
  }
}
