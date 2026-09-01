package server

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

// serveStatic is the §5 one code path for every immutable byte (bundles, LFS
// objects): strong ETag from the store version, If-None-Match → 304, Range +
// If-Range → 206/416, HEAD, immutable caching, nosniff, and the accel-offload
// edge contract.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, key, contentType string) {
	ctx := r.Context()
	meta, err := s.store.Head(ctx, key)
	if err != nil || meta == nil {
		if store.IsNotFound(err) || meta == nil {
			plainStatus(w, http.StatusNotFound, "not found")
			return
		}
		plainStatus(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	version := string(meta.Version)
	size := meta.Size

	h := w.Header()
	h.Set("ETag", `"`+version+`"`)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")

	// Accel offload (edge contract, §8.6): accel_redirect on, TCP peer
	// loopback, not HEAD, and the store can produce a URL → 200 + headers,
	// never a body. When hit directly (no X-Walgit-Capabilities) the server
	// assumes nothing and streams bytes itself.
	if s.cfg.Server.AccelRedirect && r.Method != http.MethodHead && isLoopbackHost(remoteHost(r)) {
		if at, aerr := s.store.AccelTarget(ctx, key); aerr == nil && at != nil {
			h.Set("X-Accel-Redirect", "/_store/")
			h.Set("X-Walgit-Store-Url", at.URL)
			if at.Authorization != "" {
				h.Set("X-Walgit-Store-Authorization", at.Authorization)
			}
			h.Set("X-Walgit-Store-Key", pctEncode(key))
			h.Set("X-Walgit-Etag", `"`+version+`"`)
			h.Del("Accept-Ranges")
			h.Del("Content-Type")
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// Conditional: If-None-Match → 304 (no body).
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatchHeader(inm, version) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Range + If-Range.
	rng := r.Header.Get("Range")
	if rng != "" && r.Method != http.MethodHead {
		if ir := r.Header.Get("If-Range"); ir != "" && !ifRangeMatches(ir, version) {
			rng = "" // validator differs → full 200
		}
	}
	if rng == "" {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		s.streamRange(ctx, w, key, store.GetOptions{})
		return
	}
	start, end, ok := parseRange(rng, size)
	if !ok {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		plainStatus(w, http.StatusRequestedRangeNotSatisfiable, "range not satisfiable")
		return
	}
	h.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+
		"/"+strconv.FormatInt(size, 10))
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	s.streamRange(ctx, w, key, store.GetOptions{Range: &[2]int64{start, end + 1}})
}

// streamRange copies the store object body; errors mid-stream are logged (the
// status is already sent).
func (s *Server) streamRange(ctx context.Context, w http.ResponseWriter, key string, opts store.GetOptions) {
	res, err := s.store.Get(ctx, key, opts)
	if err != nil {
		s.log.Warn("static read failed", "key", key, "err", err)
		return
	}
	obj, ok := res.(store.Object)
	if !ok {
		return // NotModified after a 304 above cannot happen
	}
	defer obj.Body.Close()
	_, _ = io.Copy(w, obj.Body)
}

// etagMatchHeader compares If-None-Match against the strong ETag.
func etagMatchHeader(header, version string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, "W/")
		p = strings.TrimPrefix(p, "w/")
		p = strings.Trim(p, `"`)
		if p == version {
			return true
		}
	}
	return false
}

// ifRangeMatches: the validator differs → serve the full 200 (§5).
func ifRangeMatches(ir, version string) bool {
	return etagMatchHeader(ir, version)
}

// serveLFSObject implements GET|HEAD …/info/lfs/objects/{oid}: the static
// contract with Content-Type: application/octet-stream, plus the upstream
// read-through (§6.3) honoring ?size=N.
func (s *Server) serveLFSObject(w http.ResponseWriter, r *http.Request, id git.RepoId, oid string) {
	key := lfsKey(id, oid)
	// Upstream read-through: upstream.lfs configured and the object is not in
	// our store → spool from upstream while streaming (§6.3).
	if s.cfg.Upstream.Lfs != "" {
		if meta, _ := s.store.Head(r.Context(), key); meta == nil {
			s.lfsReadThrough(w, r, id, oid, key)
			return
		}
	}
	s.serveStatic(w, r, key, "application/octet-stream")
}
