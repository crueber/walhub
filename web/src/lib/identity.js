// web/src/lib/identity.js — Forgejo #371: navbar identity model.
//
// Pure functions over the two auth payloads the shell already fetches:
//   me        — GET /api/v1/me → {principal, write, anonymous, admin,
//               avatar_url?} (avatar_url is the stable user-avatar URL,
//               omitted when the user has none — Forgejo #376)
//               (null when the fetch 401s/throws: signed out)
//   discovery — GET /api/v1 → {auth: {mode, browser_login, login_url}}
//               (null while loading/failed: legacy nav, no identity)
//
// navModel({me, discovery}, currentPath) → the header's auth surface:
//   mode            effective auth mode ("none"|"token"|"oidc"|"")
//   signedIn        identity menu applies (never in none mode — there is
//                   nothing to log in to, and the none principal is anon)
//   showLogin       Login button (signed out + browser flow available)
//   loginHref       "/_auth/login?next=<current>" ("" when hidden)
//   showIdentity    avatar-or-username control + dropdown
//   menuItems       [{kind, label, href}] in dropdown order
//   showKeysInNav / showInvitationsInNav / showSetupInNav
//                   primary-nav visibility (signed-in users find
//                   keys/invitations/setup in the menu; setup stays in the
//                   nav in none mode so first-run stays discoverable)
//
// Headless-testable under node --test; App.jsx + IdentityMenu.jsx keep the
// DOM thin. No new deps.

/** Effective auth mode, "" when discovery hasn't loaded/failed. */
export function authMode(discovery) {
  const m = discovery?.auth?.mode;
  return m === "none" || m === "token" || m === "oidc" ? m : "";
}

/** Browser login renders only when the discovery block says the flow works. */
export function browserLogin(discovery) {
  return discovery?.auth?.browser_login === true;
}

/** Flow entry advertised by discovery; "" when login is unavailable. */
export function loginURL(discovery) {
  const u = discovery?.auth?.login_url;
  return typeof u === "string" && u !== "" ? u : "";
}

/** Signed in: a non-anonymous principal outside none mode. */
export function isSignedIn(me, mode) {
  return !!me && me.anonymous === false && mode !== "none" && mode !== "";
}

/** Setup belongs in the menu only for users who can use it: host admins
 * (setupAccess admits admins outside none mode), or none mode itself
 * (open — though the menu never renders there). */
export function canUseSetup(me, mode) {
  return mode === "none" || me?.admin === true;
}

/**
 * menuItems(me, mode) → dropdown entries in order: profile, keys,
 * invitations, setup (admin/setup-capable only), logout. Hrefs are plain
 * strings — the component renders profile/keys/invitations/setup as router
 * links and logout as a plain anchor (GET /_auth/logout clears the session
 * cookie server-side and 302s home).
 */
export function menuItems(me, mode) {
  const name = me?.principal ?? "";
  const items = [
    { kind: "profile", label: "Your profile", href: `/${encodeURIComponent(name)}` },
    { kind: "keys", label: "Keys", href: "/keys" },
    { kind: "invitations", label: "Invitations", href: "/invitations" },
  ];
  if (canUseSetup(me, mode)) {
    items.push({ kind: "setup", label: "Setup", href: "/setup" });
  }
  items.push({ kind: "logout", label: "Log out", href: "/_auth/logout?next=/" });
  return items;
}

/**
 * navModel({me, discovery}, currentPath) → the full header auth surface.
 * currentPath feeds the login ?next= return target (defaults to "/").
 */
export function navModel({ me = null, discovery = null } = {}, currentPath = "/") {
  const mode = authMode(discovery);
  const signedIn = isSignedIn(me, mode);
  const canLogin = browserLogin(discovery) && loginURL(discovery) !== "";
  const showLogin = !signedIn && mode !== "none" && mode !== "" && canLogin;
  const next = currentPath || "/";
  const loginHref = showLogin ? `${loginURL(discovery)}?next=${encodeURIComponent(next)}` : "";
  const showIdentity = signedIn;
  return {
    mode,
    signedIn,
    showLogin,
    loginHref,
    showIdentity,
    username: signedIn ? (me.principal ?? "") : "",
    // Forgejo #376: the navbar avatar. me.avatar_url is the stable
    // user-avatar URL ("?v=" cache-busted); "" renders the username
    // fallback (IdentityMenu's optional-avatar prop).
    avatarUrl: signedIn ? (me.avatar_url || "") : "",
    menuItems: signedIn ? menuItems(me, mode) : [],
    // Primary nav: signed-in users (outside none mode) find
    // keys/invitations/setup in the menu; everyone else keeps today's nav.
    showKeysInNav: !signedIn,
    showInvitationsInNav: !signedIn,
    showSetupInNav: !signedIn || mode === "none",
  };
}
