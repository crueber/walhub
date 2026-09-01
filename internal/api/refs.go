package api

import (
	"net/http"
	"strconv"
	"strings"
)

// acceptsSSE reports whether the request's Accept contains text/event-stream.
func acceptsSSE(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// open resolves the repo, gates auth, and performs the per-request refs sync
// (§2 consistency: after a push is acknowledged the next API call on any node
// reflects it). On failure the response is already written.
func (h *handlers) open(w http.ResponseWriter, r *http.Request, level AuthLevel) bool {
	if !h.env.gate(w, r, level) {
		return false
	}
	if h.env.Repo == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo view not configured")
		return false
	}
	if err := h.env.Repo.Sync(r.Context(), RepoOf(r), SyncRefs); err != nil {
		mapViewErr(w, err)
		return false
	}
	return true
}

// baseURL is the public base: server.public_url, else the request Host (§9.1).
func (e *Env) baseURL(r *http.Request) string {
	if e.Cfg != nil && e.Cfg.Server.PublicURL != "" {
		return strings.TrimSuffix(e.Cfg.Server.PublicURL, "/")
	}
	scheme := "http"
	if r != nil && r.TLS != nil {
		scheme = "https"
	}
	host := ""
	if r != nil {
		host = r.Host
	}
	return scheme + "://" + host
}

// --- GET …/refs (§9.2, O(1) head) --------------------------------------------------

func (h *handlers) refsHead(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	head, ok, err := h.env.Repo.Head(r.Context(), RepoOf(r))
	if err != nil {
		mapViewErr(w, err)
		return
	}
	var payload struct {
		Head *Ref `json:"head"`
	}
	etag := ""
	if ok {
		payload.Head = &head
		etag = head.SHA
	}
	writeCached(w, r, ccSWR, etag, http.StatusOK, payload)
}

// --- GET …/refs/{branches|tags} (§9.2 page; §7 SSE dialect) ------------------------

type refPage struct {
	Refs []Ref `json:"refs"`
	More bool  `json:"more"`
}

func (h *handlers) refsList(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	ns := r.PathValue("ns")
	var namespace string
	switch ns {
	case "branches":
		namespace = "heads"
	case "tags":
		namespace = "tags"
	default:
		writePlain(w, http.StatusNotFound, "unknown ref namespace: "+ns)
		return
	}
	q, ok := parseRefQuery(w, r)
	if !ok {
		return
	}
	refs, more, err := h.env.Repo.RefList(r.Context(), RepoOf(r), namespace, q)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	if acceptsSSE(r) {
		h.streamRefs(w, refs, more)
		return
	}
	writeCached(w, r, ccSWR, "", http.StatusOK, refPage{Refs: nonNil(refs), More: more})
}

// streamRefs writes the §7 dialect: `event: ref` per match, terminal
// `event: done` {"more":<bool>} — unbuffered, no opener, no keepalives.
func (h *handlers) streamRefs(w http.ResponseWriter, refs []Ref, more bool) {
	s, ok := newRefStream(w)
	if !ok {
		writePlain(w, http.StatusNotAcceptable, "streaming unsupported")
		return
	}
	for _, ref := range refs {
		if !s.packet("ref", mustJSON(struct {
			Name string `json:"name"`
			SHA  string `json:"sha"`
		}{Name: ref.Name, SHA: ref.SHA})) {
			return // client gone mid-stream; the next request revalidates
		}
	}
	s.packet("done", mustJSON(struct {
		More bool `json:"more"`
	}{More: more}))
}

// parseRefQuery reads ?prefix=&q=&after=&n= (§9.2: n default 100, max 1000).
func parseRefQuery(w http.ResponseWriter, r *http.Request) (RefQuery, bool) {
	qs := r.URL.Query()
	q := RefQuery{
		Prefix: qs.Get("prefix"),
		Q:      qs.Get("q"),
		After:  qs.Get("after"),
		N:      100,
	}
	if s := qs.Get("n"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writePlain(w, http.StatusBadRequest, "invalid n")
			return q, false
		}
		if n > 1000 {
			n = 1000
		}
		q.N = n
	}
	return q, true
}
