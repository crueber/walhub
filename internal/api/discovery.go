package api

import (
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/sizecatalog"
)

// --- GET /api/v1 (§8 discovery; no phantom routes, derived from the table) ----------

type discoveryAuth struct {
	Bearer       bool   `json:"bearer"`
	Setup        string `json:"setup"`
	Browser      string `json:"browser"`
	Authenticate string `json:"authenticate"`
	// BrowserLogin advertises the #344 login entry: true when the instance
	// can start the OIDC browser flow (mode=oidc + the session/client
	// trio), so the SPA renders the "Log in with OIDC" button only when it
	// works. LoginURL is the flow entry (/_auth/login); empty when
	// BrowserLogin is false.
	BrowserLogin bool   `json:"browser_login"`
	LoginURL     string `json:"login_url"`
	// Mode is the instance auth mode ("none"|"token"|"oidc", #371): the
	// navbar keys off it — no Login button and no identity menu in none
	// mode (there is nothing to log in to), the identity menu for
	// signed-in users otherwise.
	Mode string `json:"mode"`
}

// browserLoginEnabled mirrors the server's BrowserLoginEnabled gate
// (mode oidc + session secret + client id + client secret) over the shared
// config shape — this package must not import internal/server (law 8), so
// the three-line predicate lives here too. Validation (config.Validate)
// refuses mode=oidc without the trio, so false here on an oidc instance
// means a legacy config that predates the refusal.
func browserLoginEnabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	a := cfg.Server.Auth
	return a.Mode == "oidc" && a.SessionSecret != "" &&
		a.OAuthClientID != "" && a.OAuthClientSecret != ""
}

// loginURLFor is the flow entry advertised alongside BrowserLogin: the
// /_auth/login path when the browser flow can start, else empty.
func loginURLFor(cfg *config.Config) string {
	if !browserLoginEnabled(cfg) {
		return ""
	}
	return "/_auth/login"
}

// authModeOf reports the instance auth mode for the discovery auth block
// (#371). Empty config defaults to "none" (the zero-config first run).
func authModeOf(cfg *config.Config) string {
	if cfg == nil || cfg.Server.Auth.Mode == "" {
		return "none"
	}
	return cfg.Server.Auth.Mode
}

// discoveryEndpoints derives the capability list from the route table so it
// cannot drift (§8/§20.4 normative fix): only routes the router actually
// serves, deduped by template, in registration order.
func discoveryEndpoints() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, rt := range Routes(nil) {
		if !rt.Expose || rt.Template == "" {
			continue
		}
		if seen[rt.Template] {
			continue
		}
		seen[rt.Template] = true
		out = append(out, rt.Template)
	}
	// Feature-owned routes served by ExtraRoutes chained in front of the
	// core mux (docs/features/10: the /api/v1/repos/imports twins live in
	// internal/repoimport, which core must not import — 14 §14.3). They are
	// real routes, not phantoms: the composition registers both the
	// template here and the handler there, in the same change (law 12).
	exposedMu.Lock()
	extra := append([]string{}, exposedExtra...)
	exposedMu.Unlock()
	for _, tmpl := range extra {
		if seen[tmpl] {
			continue
		}
		seen[tmpl] = true
		out = append(out, tmpl)
	}
	return out
}

// exposedExtra holds feature-owned discovery templates (see above);
// compiled-in registration only, never request-scoped.
var (
	exposedMu    sync.Mutex
	exposedExtra []string
)

// RegisterExposed appends feature-owned route templates to the discovery
// endpoints[] (14 §14.12 lane rule). Called once per template from the
// feature's composition wiring (cmd/walhub); duplicates are dropped at
// render time. Not concurrency-critical (startup-only), but mutex-guarded
// for -race cleanliness.
func RegisterExposed(templates ...string) {
	exposedMu.Lock()
	defer exposedMu.Unlock()
	exposedExtra = append(exposedExtra, templates...)
}

func (h *handlers) discovery(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthOpen) {
		return
	}
	writeCached(w, r, ccNoCache, "", http.StatusOK, struct {
		Version     int           `json:"version"`
		Base        string        `json:"base"`
		BrowserBase string        `json:"browser_base"`
		SDK         string        `json:"sdk"`
		Auth        discoveryAuth `json:"auth"`
		Endpoints   []string      `json:"endpoints"`
	}{
		Version:     1,
		Base:        "/api/v1",
		BrowserBase: "/api/v1",
		SDK:         "/repos.js",
		Auth: discoveryAuth{
			Bearer:       true,
			Setup:        "/services/setup.json",
			Browser:      "/api-browser/v1",
			Authenticate: "/api/v1/authenticate",
			BrowserLogin: browserLoginEnabled(h.env.Cfg),
			LoginURL:     loginURLFor(h.env.Cfg),
			Mode:         authModeOf(h.env.Cfg),
		},
		Endpoints: discoveryEndpoints(),
	})
}

// --- GET /api/v1/me (§13) -------------------------------------------------------------

func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	p := h.env.PrincipalOf(r)
	avatarURL := ""
	if h.env.Avatars != nil && !p.Anonymous {
		avatarURL = h.env.Avatars.UserAvatarURL(r.Context(), p.Name)
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, struct {
		Principal string `json:"principal"`
		Write     bool   `json:"write"`
		Anonymous bool   `json:"anonymous"`
		// Admin (#371) tells the navbar whether the Setup entry belongs
		// in the identity menu — setupAccess admits host admins (open
		// while mode=none, where the menu never renders anyway).
		Admin bool `json:"admin"`
		// AvatarURL (#376) is the stable user-avatar URL ("?v="
		// cache-busted); omitted when the user has none (the navbar
		// renders the username fallback) or the surface is unwired.
		AvatarURL string `json:"avatar_url,omitempty"`
	}{Principal: p.Name, Write: p.Write, Anonymous: p.Anonymous, Admin: p.Admin, AvatarURL: avatarURL})
}

// --- GET /api/v1/owners, /api/v1/owners/{owner}/repos (§8, from the STORE) ------------

func (h *handlers) owners(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	if h.env.Repos == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	// Forgejo #345: the flag gate above admits the caller (anonymous only
	// when anonymous_read is true — the flag's kept non-repo meaning);
	// the CONTENT is visibility-filtered: owners with no repo readable
	// by this caller are omitted, so no private repo leaks via
	// membership. Unfiltered callers (nil Access, host admin)
	// take the legacy Owners() walk, byte-identical trips.
	p := h.env.PrincipalOf(r)
	var names []string
	var allow map[string]map[string]bool // nil when unfiltered
	if h.env.unfiltered(p) {
		var err error
		names, err = h.env.Repos.Owners(r.Context())
		if err != nil {
			mapViewErr(w, err)
			return
		}
	} else {
		byOwner, berr := h.env.VisibleReposByOwner(r.Context(), p)
		if berr != nil {
			mapViewErr(w, berr)
			return
		}
		names = make([]string, 0, len(byOwner))
		allow = make(map[string]map[string]bool, len(byOwner))
		for owner, repos := range byOwner {
			names = append(names, owner)
			set := make(map[string]bool, len(repos))
			for _, n := range repos {
				set[n] = true
			}
			allow[owner] = set
		}
		sort.Strings(names)
	}
	// sort=activity orders the frozen []string by the derived per-owner
	// max-commit rollup (Forgejo #283 — the server ranks over ALL owners
	// before the client's MAX_OWNERS slice, so an active owner past the
	// name-cap still surfaces). Default (no sort) is byte-identical to the
	// legacy store order: no catalog read, zero added trips. Absent
	// catalog degrades to name order; corrupt catalog is a 503 (the bucket
	// is wrong) — the same contract as ownerReposDetailed.
	if sortKey, order := ownerSortParams(r); sortKey == "activity" {
		rollups, rerr := ownerRollups(r, h, allow)
		if rerr != nil {
			mapViewErr(w, rerr)
			return
		}
		names = sizecatalog.SortOwners(names, rollups, sortKey, order)
	}
	writeCached(w, r, ccSWR, "", http.StatusOK, nonNil(names))
}

func (h *handlers) ownerRepos(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	// 200 [] for an unknown owner — never 404 (§8). Forgejo #345: the
	// rows are visibility-filtered (anonymous ⇒ public only), so a
	// private name never leaks through this listing.
	repos, err := h.env.VisibleRepos(r.Context(), r.PathValue("owner"), h.env.PrincipalOf(r))
	if err != nil {
		mapViewErr(w, err)
		return
	}
	writeCached(w, r, ccSWR, "", http.StatusOK, nonNil(repos))
}

// --- GET /services/api/instance (§8 — "this machine" for UI footers) ------------------

type instanceBody struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Revision    string   `json:"revision"`
	Instance    string   `json:"instance"`
	Version     string   `json:"version"`
	Roles       []string `json:"roles"`
	Disk        string   `json:"disk"`
	Shape       string   `json:"shape"`
	CPUs        int      `json:"cpus"`
	MemoryBytes uint64   `json:"memory_bytes"`
}

func (h *handlers) instance(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthOpen) {
		return
	}
	e := h.env
	roles := []string{}
	disk := ""
	if e.Cfg != nil {
		roles = nonNil(e.Cfg.Server.Roles)
		disk = e.Cfg.Maintenance.Disk
	}
	name := e.Hostname
	writeCached(w, r, ccNoStore, "", http.StatusOK, instanceBody{
		Kind:        "walhub",
		Name:        name,
		Revision:    e.Version,
		Instance:    name,
		Version:     e.Version,
		Roles:       roles,
		Disk:        disk,
		CPUs:        numCPU(),
		MemoryBytes: totalMemory(),
	})
}

// numCPU and totalMemory are the "this machine" facts (§8): CPU count and
// total memory (MemTotal from /proc/meminfo; 0 when unavailable).
func numCPU() int { return runtime.NumCPU() }

func totalMemory() uint64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					return n * 1024
				}
			}
		}
	}
	return 0
}
