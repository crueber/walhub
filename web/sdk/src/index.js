/**
 * repos.js SDK entry (12_web_ui.md §1.0/§1.1).
 *
 * Public surface: named exports `ReposClient` + `ReposError`, and a default
 * export that is the DEFAULT CLIENT instance — so the external-consumer
 * pattern of §5b works: `import repos from "…/repos.js";
 * repos.configure({token}).repo("o/r").tree(…)`. `window.repos` is set only
 * if unset. ES modules are the only distribution (no IIFE/global build, no
 * .mjs twin, pre-1.0 no-compat).
 */
import { ReposClient } from "./core.js";
import { ReposError } from "./errors.js";
import { parseFrame, readSse } from "./sse.js";

export { ReposClient, ReposError, parseFrame, readSse };

// Default client: one per document, shared by the SPA and external consumers.
let defaultClient = new ReposClient();
if (typeof globalThis.window === "object") {
  const existing = globalThis.repos;
  if (existing instanceof ReposClient) defaultClient = existing;
  else globalThis.repos = defaultClient;
}

export default defaultClient;
