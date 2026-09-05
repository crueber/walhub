// web/src/lib/clone.js — clone-URL builders for the repo-header clone menu
// (issue #37). Pure and headless-testable: no DOM, no Solid, no fetch.
//
// The server serves exactly two git transports, both verified against the
// served surface (never invented client-side):
// - HTTP(S): summary.clone_url (internal/api/summary.go: base + /owner/name.git),
//   verbatim — the scheme is whatever the server advertised (https behind TLS,
//   plain http otherwise). The pill label derives from this URL (issue #124):
//   the pill and the text must AGREE and the command must WORK, so an http://
//   advertisement is never upgraded to https:// for display — a plain-http
//   server has no TLS and the upgraded command would be broken.
// - SSH:   internal/sshd (docs/go/17_ssh.md) speaking the standard git
//   transports at ssh://git@host[:port]/owner/repo.git (README, the
//   bind_ssh_test.go ssh:// bases, the /keys page's ssh:// copy).
// The server never advertises its SSH listen port to the browser, so the SSH
// URL reuses the HTTP(S) URL's hostname at the DEFAULT ssh port (no port
// segment — the HTTP port is never the SSH port, so carrying it over would
// be wrong). Deployments on a custom port (compose rigs: 2222) adjust the
// port or use ~/.ssh/config.

export function httpsCloneUrl(summary, full, origin) {
  return summary?.clone_url ?? `${origin}/${full}.git`;
}

// httpProtoLabel names the HTTP(S)-transport pill from the advertised URL:
// "HTTPS" for https://, "HTTP" otherwise (http:// and any non-URL fallback —
// the fallback still clones over plain http, so HTTP is the honest label).
export function httpProtoLabel(url) {
  return /^https:/i.test(String(url ?? "")) ? "HTTPS" : "HTTP";
}

export function sshCloneUrl(httpsUrl, full, host = "") {
  let hostname = host;
  try {
    hostname = new URL(httpsUrl).hostname || host;
  } catch {
    // Unparseable URL (or no URL constructor): fall back to the page host.
  }
  return `ssh://git@${hostname || "localhost"}/${full}.git`;
}

export function cloneCommand(url) {
  return `git clone ${url}`;
}

// copyText copies text and reports success. Clipboard API first (needs a
// secure context plus a user gesture — both true for a dialog click); the
// legacy execCommand path covers older engines (the caller selects the
// textbox first so there is something to copy). False means neither worked —
// the caller keeps the textbox selected so Ctrl+C still rescues the copy.
export async function copyText(text) {
  try {
    const nav = globalThis.navigator;
    if (nav?.clipboard?.writeText) {
      await nav.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Fall through to execCommand.
  }
  try {
    const doc = globalThis.document;
    if (doc?.execCommand) return doc.execCommand("copy") === true;
  } catch {
    return false;
  }
  return false;
}
