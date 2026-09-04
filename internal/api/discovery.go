package api

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// --- GET /api/v1 (§8 discovery; no phantom routes, derived from the table) ----------

type discoveryAuth struct {
	Bearer       bool   `json:"bearer"`
	Setup        string `json:"setup"`
	Browser      string `json:"browser"`
	Authenticate string `json:"authenticate"`
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
	writeCached(w, r, ccNoStore, "", http.StatusOK, struct {
		Principal string `json:"principal"`
		Write     bool   `json:"write"`
		Anonymous bool   `json:"anonymous"`
	}{Principal: p.Name, Write: p.Write, Anonymous: p.Anonymous})
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
	names, err := h.env.Repos.Owners(r.Context())
	if err != nil {
		mapViewErr(w, err)
		return
	}
	writeCached(w, r, ccSWR, "", http.StatusOK, nonNil(names))
}

func (h *handlers) ownerRepos(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	// 200 [] for an unknown owner — never 404 (§8).
	repos, err := h.env.Repos.Repos(r.Context(), r.PathValue("owner"))
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
