package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
)

// Route is one entry of the table-driven registration consumed by
// internal/server (07_api.md §1). For repo-scoped routes, Sub is the pattern
// AFTER the lane ("/{owner}/{repo}/api" or "/api-browser"), matched against
// raw-path segments split before decoding; tokens: literal, "{name}" (one
// segment), "{name...}" (the remaining segments, ≥1). NonRepo routes carry a
// full path in Sub. Template is the full path template used by the discovery
// document; Expose lists it in the discovery endpoints[].
type Route struct {
	Method   string
	Sub      string
	Template string
	Handler  http.HandlerFunc
	Auth     AuthLevel
	Expose   bool
	NonRepo  bool
}

// handlers is the receiver behind the route table; the env is injected by the
// dispatcher (Mount or internal/server).
type handlers struct{ env *Env }

// Routes is the complete §3 endpoint table. Both lanes answer the same
// handlers (D27); the dispatcher resolves the lane before dispatch. The
// discovery document is derived from this table, so it cannot drift (§8).
func Routes(e *Env) []Route {
	h := &handlers{env: e}
	return []Route{
		// --- non-repo twins (§3 lane note: /api/v1 + /api-browser/v1, plus the
		// /services/api twins for owners; instance is services-only) ----------
		{Method: "GET", Sub: "/api/v1/me", Template: "/api/v1/me", Handler: h.me, Auth: AuthRead, Expose: true, NonRepo: true},
		{Method: "GET", Sub: "/api/v1/ssh-keys", Template: "/api/v1/ssh-keys", Handler: h.sshKeysList, Auth: AuthRead, Expose: true, NonRepo: true},
		{Method: "POST", Sub: "/api/v1/ssh-keys", Template: "/api/v1/ssh-keys", Handler: h.sshKeysAdd, Auth: AuthRead, NonRepo: true},
		{Method: "DELETE", Sub: "/api/v1/ssh-keys/{fp}", Template: "/api/v1/ssh-keys/{fp}", Handler: h.sshKeysDelete, Auth: AuthRead, NonRepo: true},
		{Method: "GET", Sub: "/api-browser/v1/me", Handler: h.me, Auth: AuthRead, NonRepo: true},
		{Method: "GET", Sub: "/api/v1/owners", Template: "/api/v1/owners", Handler: h.owners, Auth: AuthRead, Expose: true, NonRepo: true},
		{Method: "GET", Sub: "/api-browser/v1/owners", Handler: h.owners, Auth: AuthRead, NonRepo: true},
		{Method: "GET", Sub: "/services/api/owners", Handler: h.owners, Auth: AuthRead, NonRepo: true},
		{Method: "GET", Sub: "/api/v1/owners/{owner}/repos", Template: "/api/v1/owners/{owner}/repos", Handler: h.ownerRepos, Auth: AuthRead, Expose: true, NonRepo: true},
		{Method: "GET", Sub: "/api-browser/v1/owners/{owner}/repos", Handler: h.ownerRepos, Auth: AuthRead, NonRepo: true},
		{Method: "GET", Sub: "/services/api/owners/{owner}/repos", Handler: h.ownerRepos, Auth: AuthRead, NonRepo: true},
		{Method: "GET", Sub: "/services/api/instance", Handler: h.instance, Auth: AuthOpen, NonRepo: true},

		// --- repo reads (§9) ---------------------------------------------------
		{Method: "GET", Sub: "", Template: "/{owner}/{repo}/api", Handler: h.summary, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "refs", Template: "/{owner}/{repo}/api/refs", Handler: h.refsHead, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "refs/{ns}", Template: "/{owner}/{repo}/api/refs/{branches|tags}", Handler: h.refsList, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "resolve", Template: "/{owner}/{repo}/api/resolve/{ref}", Handler: h.resolve, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "resolve/{rest...}", Handler: h.resolve, Auth: AuthRead},
		{Method: "GET", Sub: "tree/{rev}", Template: "/{owner}/{repo}/api/tree/{rev}", Handler: h.tree, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "tree/{rev}/{path...}", Handler: h.tree, Auth: AuthRead},
		{Method: "GET", Sub: "blob/{rev}/{path...}", Template: "/{owner}/{repo}/api/blob/{rev}/{path}", Handler: h.blob, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "commits", Template: "/{owner}/{repo}/api/commits", Handler: h.commits, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "commit/{sha}", Template: "/{owner}/{repo}/api/commit/{sha}", Handler: h.commitDetail, Auth: AuthRead, Expose: true},

		// --- policy (§10) --------------------------------------------------------
		{Method: "GET", Sub: "policy", Template: "/{owner}/{repo}/api/policy", Handler: h.policyGet, Auth: AuthRead, Expose: true},
		{Method: "PUT", Sub: "policy", Handler: h.policyPut, Auth: AuthAdmin},
		{Method: "DELETE", Sub: "policy", Handler: h.policyDelete, Auth: AuthAdmin},
		{Method: "POST", Sub: "policy/validate", Handler: h.policyValidate, Auth: AuthRead},
		{Method: "POST", Sub: "policy/dry-run", Handler: h.policyDryRun, Auth: AuthRead},

		// --- settings (§11) -------------------------------------------------------
		{Method: "GET", Sub: "settings", Template: "/{owner}/{repo}/api/settings", Handler: h.settingsGet, Auth: AuthRead, Expose: true},
		{Method: "PUT", Sub: "settings", Handler: h.settingsPut, Auth: AuthAdmin},
		{Method: "DELETE", Sub: "settings", Handler: h.settingsDelete, Auth: AuthAdmin},
		{Method: "GET", Sub: "settings/effective", Handler: h.settingsEffective, Auth: AuthRead},
		{Method: "GET", Sub: "settings/history", Handler: h.settingsHistory, Auth: AuthRead},
		{Method: "GET", Sub: "settings/describe", Handler: h.settingsDescribe, Auth: AuthRead},
		{Method: "POST", Sub: "settings/validate", Handler: h.settingsValidate, Auth: AuthRead},

		// --- overview, ops, tasks (§12) ---------------------------------------------
		{Method: "GET", Sub: "overview", Template: "/{owner}/{repo}/api/overview", Handler: h.overview, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "ops", Template: "/{owner}/{repo}/api/ops", Handler: h.opsList, Auth: AuthRead, Expose: true},
		{Method: "POST", Sub: "ops/{op}", Handler: h.opStart, Auth: AuthWrite},
		{Method: "GET", Sub: "tasks", Template: "/{owner}/{repo}/api/tasks", Handler: h.tasksList, Auth: AuthRead, Expose: true},
		{Method: "GET", Sub: "tasks/{id}", Handler: h.taskGet, Auth: AuthRead},

		// --- repo lifecycle (§9.1: PUT creates on the lane root, DELETE deletes) ----
		{Method: "PUT", Sub: "", Handler: h.repoPut, Auth: AuthWrite},
		{Method: "DELETE", Sub: "", Handler: h.repoDelete, Auth: AuthAdmin},
	}
}

// --- pattern matching ----------------------------------------------------------

// matchRoute matches sub (decoded segments) against a route's Sub pattern.
// Returns the captured params, or nil when it does not match.
func matchRoute(pattern string, sub []string) map[string]string {
	if pattern == "" {
		if len(sub) == 0 {
			return map[string]string{}
		}
		return nil
	}
	toks := strings.Split(pattern, "/")
	for i, tok := range toks {
		if strings.HasSuffix(tok, "...}") {
			name := tok[1 : len(tok)-4]
			if i != len(toks)-1 || len(sub) < i+1 {
				return nil
			}
			params := map[string]string{}
			for j, t := range toks[:i] {
				params[paramName(t)] = sub[j]
			}
			params[name] = strings.Join(sub[i:], "/")
			return params
		}
		if i >= len(sub) {
			return nil
		}
		if !strings.HasPrefix(tok, "{") {
			if tok != sub[i] {
				return nil
			}
			continue
		}
	}
	if len(sub) != len(toks) {
		return nil
	}
	params := map[string]string{}
	for i, tok := range toks {
		params[paramName(tok)] = sub[i]
	}
	return params
}

func paramName(tok string) string {
	if strings.HasSuffix(tok, "...}") {
		return tok[1 : len(tok)-4]
	}
	return strings.Trim(tok, "{}")
}

// --- dispatcher -----------------------------------------------------------------

// Dispatch answers one request with the repo-scoped table: strips the lane,
// strips the .git suffix, parses the repo id (bad id → 404), matches the
// table, and enforces methods itself (06_server_http.md §3.1). The wildcard
// dispatcher is also the last-resort 404 for non-repo-shaped junk.
func Dispatch(e *Env, w http.ResponseWriter, r *http.Request) {
	// Raw-path segments; each is decoded individually downstream (§2: never
	// unescape a joined multi-segment string).
	segs := rawSegments(r)
	if len(segs) < 2 {
		writePlain(w, http.StatusNotFound, "not found")
		return
	}
	owner, repoSeg := segs[0], segs[1]
	rest := segs[2:]
	if strings.HasSuffix(repoSeg, ".git") {
		repoSeg = strings.TrimSuffix(repoSeg, ".git")
	}
	id, err := git.ParseRepoId(owner + "/" + repoSeg)
	if err != nil {
		writePlain(w, http.StatusNotFound, "not found")
		return
	}

	// Lane resolution happens BEFORE dispatch (07_api.md §1): strip
	// "/{owner}/{repo}[.git]/api" or "/api-browser"; handlers are lane-agnostic.
	lane := LaneAPI
	if len(rest) > 0 && (rest[0] == "api" || rest[0] == "api-browser") {
		if rest[0] == "api-browser" {
			lane = LaneAPIBrowser
		}
		rest = rest[1:]
	}

	// Repo-scoped sub segments, decoded one at a time.
	sub := make([]string, 0, len(rest))
	for _, s := range rest {
		d, derr := decodeSegment(s)
		if derr != nil {
			writePlain(w, http.StatusBadRequest, "invalid path encoding")
			return
		}
		sub = append(sub, d)
	}

	routes := Routes(e)
	var allow []string
	for i := range routes {
		rt := &routes[i]
		if rt.NonRepo {
			continue
		}
		params := matchRoute(rt.Sub, sub)
		if params == nil {
			continue
		}
		if rt.Method != r.Method {
			if !contains(allow, rt.Method) {
				allow = append(allow, rt.Method)
			}
			continue
		}
		r2 := inject(r, lane, id, params)
		if !e.gate(w, r2, rt.Auth) {
			return
		}
		// The identity require_read hook (01 §4.1): every repo-scoped read
		// endpoint consults access.json visibility + role resolution after
		// the flag gate. Nil Access → legacy behavior, unchanged.
		if !rt.NonRepo && rt.Auth == AuthRead && e.Access != nil {
			if aerr := e.Access.CheckRead(r2.Context(), id.Owner, id.Name, e.PrincipalOf(r2)); aerr != nil {
				mapAccessErr(w, aerr)
				return
			}
		}
		rt.Handler(w, r2)
		return
	}
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writePlain(w, http.StatusNotFound, "not found")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// inject clones the request with the lane, repo id, and pattern params set as
// path values (r.PathValue reads them back).
func inject(r *http.Request, lane Lane, id git.RepoId, params map[string]string) *http.Request {
	ctx := context.WithValue(r.Context(), laneCtxKey{}, lane)
	r2 := r.WithContext(ctx)
	r2.SetPathValue("owner", id.Owner)
	r2.SetPathValue("repo", id.Name)
	for k, v := range params {
		r2.SetPathValue(k, v)
	}
	return r2
}

// laneCtxKey carries the answering lane to handlers.
type laneCtxKey struct{}

// LaneOf returns the answering lane for a repo-scoped request.
func LaneOf(r *http.Request) Lane {
	if l, ok := r.Context().Value(laneCtxKey{}).(Lane); ok {
		return l
	}
	return LaneAPI
}

// RepoOf returns the parsed repo id for a repo-scoped request.
func RepoOf(r *http.Request) git.RepoId {
	return git.RepoId{Owner: r.PathValue("owner"), Name: r.PathValue("repo")}
}

// decodeSegment decodes one path segment (per-segment encoding, 07_api.md §2).
func decodeSegment(s string) (string, error) {
	return url.PathUnescape(s)
}

// --- mounting -------------------------------------------------------------------

// Mount builds a self-contained http.Handler over the table: every NonRepo
// row registered on an http.ServeMux (static beats the catch-all) plus the
// §3.2 fallback dispatcher for the repo wildcard, and the discovery document
// on both lane roots. internal/server replaces this wiring with chi while
// consuming the same table; Mount exists so the package is testable end to
// end and serves until the chi tree lands.
func Mount(e *Env) http.Handler {
	e.Ready()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1", (&handlers{env: e}).discovery)
	mux.HandleFunc("GET /api-browser/v1", (&handlers{env: e}).discovery)
	for _, rt := range Routes(e) {
		if rt.NonRepo {
			mux.HandleFunc(rt.Method+" "+rt.Sub, rt.Handler)
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		Dispatch(e, w, r)
	})
	return mux
}
