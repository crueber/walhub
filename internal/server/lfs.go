package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

const lfsMediaType = "application/vnd.git-lfs+json"

// lfsDispatch routes the info/lfs/* subtree (§6).
func (s *Server) lfsDispatch(w http.ResponseWriter, r *http.Request, id git.RepoId, rest []string) {
	// rest[0] = "objects" | "verify" (caller stripped "info"/"lfs").
	switch {
	case len(rest) == 1 && rest[0] == "objects/batch":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.lfsBatch(w, r, id)
	case len(rest) == 2 && rest[0] == "objects":
		oid := rest[1]
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.lfsGet(w, r, id, oid)
		case http.MethodPut:
			s.lfsPut(w, r, id, oid)
		default:
			w.Header().Set("Allow", "GET, HEAD, PUT")
			plainStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case len(rest) == 1 && rest[0] == "verify":
		s.lfsVerify(w, r, id)
	default:
		plainStatus(w, http.StatusNotFound, "not found")
	}
}

// lfsAuth is the common LFS gate: read for download, write for upload.
// Reads additionally consult the identity require_read hook (01 §4.1).
func (s *Server) lfsAuth(w http.ResponseWriter, r *http.Request, id git.RepoId, write bool) (auth.Principal, bool) {
	p, aerr := s.authSvc.Authenticate(r, s.cfg)
	if aerr != nil {
		s.gitAuthFailure(w, r, git.ServiceUploadPack, aerr)
		return p, false
	}
	if write {
		if aerr := requireWrite(p); aerr != nil {
			s.gitAuthFailure(w, r, git.ServiceUploadPack, aerr)
			return p, false
		}
	} else if aerr := requireRead(p, s.cfg.Server.Auth.AnonymousRead); aerr != nil {
		s.gitAuthFailure(w, r, git.ServiceUploadPack, aerr)
		return p, false
	} else if aerr := s.checkReadGate(r.Context(), id.Owner, id.Name, p); aerr != nil {
		s.gitAuthFailure(w, r, git.ServiceUploadPack, aerr)
		return p, false
	}
	return p, true
}

// lfsObject is one batch API request object.
type lfsObject struct {
	OID           string               `json:"oid"`
	Size          int64                `json:"size"`
	Actions       map[string]lfsAction `json:"actions,omitempty"`
	Authenticated bool                 `json:"authenticated"`
	Err           *lfsObjectError      `json:"error,omitempty"`
}

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
}

type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lfsBatchReq struct {
	Operation string      `json:"operation"`
	Transfer  []string    `json:"transfer"`
	Objects   []lfsObject `json:"objects"`
}

// lfsBatch implements the §6.1 batch semantics table.
func (s *Server) lfsBatch(w http.ResponseWriter, r *http.Request, id git.RepoId) {
	if !strings.Contains(r.Header.Get("Accept"), "vnd.git-lfs") {
		// git-lfs always sends the media type; tolerate missing Accept anyway.
		_ = r
	}
	if _, ok := s.lfsAuth(w, r, id, false); !ok {
		return
	}
	var req lfsBatchReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		plainStatus(w, http.StatusBadRequest, "malformed batch body")
		return
	}
	if req.Operation != "upload" && req.Operation != "download" {
		plainStatus(w, http.StatusBadRequest, "operation must be upload or download")
		return
	}
	base := s.baseURL(r)
	out := make([]lfsObject, 0, len(req.Objects))
	for _, o := range req.Objects {
		out = append(out, s.lfsBatchOne(r.Context(), base, id, o, req.Operation))
	}
	writeLFSJSON(w, http.StatusOK, map[string]any{
		"transfer": "basic",
		"objects":  out,
	})
}

// lfsBatchOne applies one row of the §6.1 table. Any upstream failure is
// treated as absent — never a 5xx on the batch.
func (s *Server) lfsBatchOne(ctx context.Context, base string, id git.RepoId, o lfsObject, operation string) lfsObject {
	key := lfsKey(id, o.OID)
	meta, herr := s.store.Head(ctx, key)
	if herr == nil && meta != nil {
		// Present in our store.
		if operation == "download" {
			o.Actions = map[string]lfsAction{"download": {Href: base + lfsDownloadPath(id, o.OID)}}
			o.Authenticated = true
		}
		return o // upload: no actions (git-lfs treats present as pushed)
	}
	if s.cfg.Upstream.Lfs != "" {
		// Missing + upstream: read-through resolvable → our href (+?size=N
		// for download, the upstream batch demands exact size).
		if size, ok := s.lfsUpstreamResolve(ctx, id, o.OID); ok {
			href := base + lfsDownloadPath(id, o.OID)
			if size > 0 {
				href += "?size=" + strconv.FormatInt(size, 10)
			}
			o.Actions = map[string]lfsAction{"download": {Href: href}}
			o.Authenticated = true
			return o
		}
	}
	if operation == "upload" {
		o.Actions = map[string]lfsAction{
			"upload": {Href: base + lfsUploadPath(id, o.OID)},
			"verify": {Href: base + lfsVerifyPath(id)},
		}
		o.Authenticated = true
		return o
	}
	o.Err = &lfsObjectError{Code: 404, Message: "object does not exist"}
	return o
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.Server.PublicURL != "" {
		return strings.TrimSuffix(s.cfg.Server.PublicURL, "/")
	}
	scheme := "http"
	if s.tlsOn || r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func lfsDownloadPath(id git.RepoId, oid string) string {
	return "/" + id.String() + ".git/info/lfs/objects/" + oid
}
func lfsUploadPath(id git.RepoId, oid string) string { return lfsDownloadPath(id, oid) }
func lfsVerifyPath(id git.RepoId) string {
	return "/" + id.String() + ".git/info/lfs/verify"
}

// lfsGet streams one object (static contract; read-through spools upstream).
func (s *Server) lfsGet(w http.ResponseWriter, r *http.Request, id git.RepoId, oid string) {
	if _, ok := s.lfsAuth(w, r, id, false); !ok {
		return
	}
	// HEAD with upstream read-through → 200 + Content-Length from the
	// upstream batch (no byte fetch) (§6.3).
	if r.Method == http.MethodHead && s.storeHeadMiss(r.Context(), id, oid) && s.cfg.Upstream.Lfs != "" {
		if size, ok := s.lfsUpstreamResolve(r.Context(), id, oid); ok {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	s.serveLFSObject(w, r, id, oid)
}

func (s *Server) storeHeadMiss(ctx context.Context, id git.RepoId, oid string) bool {
	meta, _ := s.store.Head(ctx, lfsKey(id, oid))
	return meta == nil
}

// lfsPut streams the upload: size + sha256 verified BEFORE the store write;
// lfs.max_object_bytes cap → 413 (§6.2).
func (s *Server) lfsPut(w http.ResponseWriter, r *http.Request, id git.RepoId, oid string) {
	if !strings.Contains(oid, "-") || len(oid) != 64 {
		// git-lfs oids are sha256 (64 hex); accept both 40-hex (sha1) too.
		if !isHexOidLFS(oid) {
			plainStatus(w, http.StatusBadRequest, "malformed oid")
			return
		}
	}
	if _, ok := s.lfsAuth(w, r, id, true); !ok {
		return
	}
	max := int64(s.cfg.LFS.MaxObjectBytes)
	if max <= 0 {
		max = 16 << 30 // default 16 GiB
	}
	hash := sha256.New()
	// Spool to a temp file under cache.dir/lfs-spool (never buffer the
	// object in memory); reading one byte over the cap → 413.
	if err := ensureDir(s.cacheDir("lfs-spool")); err != nil {
		plainStatus(w, http.StatusServiceUnavailable, "spool unavailable")
		return
	}
	tmp, err := os.CreateTemp(s.cacheDir("lfs-spool"), "put-*")
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable, "spool unavailable")
		return
	}
	defer os.Remove(tmp.Name())
	limit := max + 1
	n, cerr := copyTee(io.LimitReader(r.Body, limit), tmp, hash)
	if int64(n) > max {
		plainStatus(w, http.StatusRequestEntityTooLarge, "object exceeds lfs.max_object_bytes")
		return
	}
	if cerr != nil {
		plainStatus(w, http.StatusBadRequest, "failed to read upload")
		return
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != oid && len(oid) == 64 {
		plainStatus(w, http.StatusBadRequest, "sha256 mismatch")
		return
	}
	size := int64(n)
	if want := r.Header.Get("X-Lfs-Expected-Size"); want != "" {
		if wn, err := strconv.ParseInt(want, 10, 64); err == nil && wn != size {
			plainStatus(w, http.StatusBadRequest, "size mismatch")
			return
		}
	}
	if _, serr := tmp.Seek(0, io.SeekStart); serr != nil {
		plainStatus(w, http.StatusInternalServerError, "spool rewind failed")
		return
	}
	key := lfsKey(id, oid)
	_, perr := s.store.Put(r.Context(), key, store.PutBody{Stream: tmp, StreamLen: size},
		store.PutOptions{ContentType: "application/octet-stream", Immutable: true})
	if perr != nil {
		plainStatus(w, http.StatusServiceUnavailable, perr.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// lfsVerify checks the recorded oid/size against what was stored (§6.2).
func (s *Server) lfsVerify(w http.ResponseWriter, r *http.Request, id git.RepoId) {
	if !strings.Contains(r.Header.Get("Content-Type"), "vnd.git-lfs") {
		// tolerate
		_ = r
	}
	if _, ok := s.lfsAuth(w, r, id, true); !ok {
		return
	}
	var req lfsObject
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		plainStatus(w, http.StatusBadRequest, "malformed verify body")
		return
	}
	meta, err := s.store.Head(r.Context(), lfsKey(id, req.OID))
	if err != nil || meta == nil {
		writeLFSJSON(w, http.StatusNotFound, map[string]any{
			"message": "object not found",
		})
		return
	}
	if meta.Size != req.Size {
		writeLFSJSON(w, http.StatusBadRequest, map[string]any{
			"message": "size mismatch",
		})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func isHexOidLFS(s string) bool {
	if len(s) != 64 && len(s) != 40 {
		return false
	}
	for i := range s {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// --- upstream read-through (§6.3) ------------------------------------------------------

// lfsUpstreamResolve runs ONE upstream batch (only missing oids, 10 s timeout,
// Basic x-access-token:<token>) and returns the exact size; any failure =
// treat as absent, never 5xx.
func (s *Server) lfsUpstreamResolve(ctx context.Context, id git.RepoId, oid string) (int64, bool) {
	token := ""
	if s.cfg.Upstream.TokenEnv != "" {
		token = os.Getenv(s.cfg.Upstream.TokenEnv)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(lfsBatchReq{
		Operation: "download",
		Objects:   []lfsObject{{OID: oid}},
	})
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		strings.TrimSuffix(s.cfg.Upstream.Lfs, "/")+"/batch", strings.NewReader(string(body)))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", lfsMediaType)
	req.Header.Set("Accept", lfsMediaType)
	if token != "" {
		req.SetBasicAuth("x-access-token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, false
	}
	defer resp.Body.Close()
	var out lfsBatchReq
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return 0, false
	}
	for _, o := range out.Objects {
		if o.OID == oid && o.Err == nil {
			if a, ok := o.Actions["download"]; ok {
				if i := strings.Index(a.Href, "?size="); i >= 0 {
					if n, err := strconv.ParseInt(a.Href[i+6:], 10, 64); err == nil {
						return n, true
					}
				}
			}
			return o.Size, true
		}
	}
	return 0, false
}

// lfsReadThrough streams upstream bytes to the client while tee-ing into a
// spool file; after a complete sha256-verified read the spool persists into
// our store. Never persists on short read/hash mismatch; a disconnecting
// client does NOT stop the persist. Owner writes; the spool is removed on
// failure. Bounded memory: io.Copy with a fixed 1 MiB buffer, sha256 via
// TeeReader in the same loop — no second pass (§6.3).
func (s *Server) lfsReadThrough(w http.ResponseWriter, r *http.Request, id git.RepoId, oid, key string) {
	ctx := r.Context()
	size, ok := s.lfsUpstreamResolve(ctx, id, oid)
	if !ok {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	token := ""
	if s.cfg.Upstream.TokenEnv != "" {
		token = os.Getenv(s.cfg.Upstream.TokenEnv)
	}
	upURL := strings.TrimSuffix(s.cfg.Upstream.Lfs, "/") + lfsDownloadPath(id, oid)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, upURL, nil)
	if err != nil {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	if token != "" {
		req.SetBasicAuth("x-access-token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	defer resp.Body.Close()

	if err := ensureDir(s.cacheDir("lfs-spool")); err != nil {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	spool, err := os.CreateTemp(s.cacheDir("lfs-spool"), "spool-*")
	if err != nil {
		plainStatus(w, http.StatusNotFound, "not found")
		return
	}
	// The handler owns one goroutine: io.Copy from upstream into a
	// MultiWriter(client, spool); the client half switches to io.Discard on
	// write error (keep reading upstream); the persist is the terminal step.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)

	clientW := writerFunc(w.Write)
	var clientErr atomic.Bool
	mw := multiWriter(func(p []byte) (int, error) {
		n1, e1 := spool.Write(p)
		if clientErr.Load() {
			return n1, e1
		}
		n2, e2 := clientW.Write(p)
		if e2 != nil {
			clientErr.Store(true)
		}
		return min(n1, n2), firstErr(e1, e2)
	})
	hash := sha256.New()
	tee := io.TeeReader(resp.Body, hash)
	_, cerr := io.CopyBuffer(mw, tee, make([]byte, 1<<20))
	_ = spool.Sync()

	// The persist decision runs to completion regardless of the client.
	go func() {
		defer os.Remove(spool.Name())
		defer spool.Close()
		got := hex.EncodeToString(hash.Sum(nil))
		st, statErr := spool.Stat()
		if cerr != nil || statErr != nil || got != oid || st.Size() != size {
			return // never persist on short read or hash mismatch
		}
		_, _ = spool.Seek(0, io.SeekStart)
		_, _ = s.store.Put(context.Background(), key,
			store.PutBody{Stream: spool, StreamLen: st.Size()},
			store.PutOptions{ContentType: "application/octet-stream", Immutable: true})
	}()
	// The handler returns after the client-abandon path completes; the
	// persist goroutine owns the spool file from here.
}

// copyTee copies src into dst while hashing; returns bytes written.
func copyTee(src io.Reader, dst io.Writer, h io.Writer) (int64, error) {
	return io.Copy(io.MultiWriter(dst, h), src)
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func multiWriter(fn func(p []byte) (int, error)) io.Writer { return writerFunc(fn) }

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// writeLFSJSON writes an LFS JSON body.
func writeLFSJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", lfsMediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

var _ = auth.ErrInvalid
