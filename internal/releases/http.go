package releases

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Wire conventions (07 §2, same as internal/api and internal/notify):
// JSON success, plain-text errors, arrays [] never null, RFC 3339 UTC,
// per-segment decoding, SWR+ETag on JSON GETs and the static contract on
// bytes, both lanes everywhere. Anonymous-denied reads get a real 401 with
// WWW-Authenticate: Bearer (never a 200 with an in-band error).

// Handler is the Seam 1 surface: every §7 releases endpoint on both lanes
// plus the byte route (HandleRepo, consulted by the server's repoDispatch
// fallback — the static uncompressed group per the 14.3 routing note).
// Composition chains it in front of the core api mux: Handle reports false
// for non-releases paths so the core mux answers.
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// principal resolves the request principal via the injected Authenticator
// (Seam 2); nil Authenticator falls back to anonymous (production always
// injects the server chain).
func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a releases route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if segs[0] == "api" || segs[0] == "api-browser" {
		return false // no top-level releases routes; all are repo-scoped
	}
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		if len(segs[3:]) >= 1 && segs[3] == "releases" {
			h.handleRepo(w, r, owner, repo, segs[3:])
			return true
		}
	}
	return false
}

// ServeHTTP answers releases routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// HandleRepo answers the byte route GET|HEAD
// /{o}/{r}/releases/{tag}/assets/{name} (the static contract, outside the
// api lanes). False when sub is not an asset byte path — the server falls
// through to its 404. sub arrives per-segment decoded (repoDispatch
// decodes before splitting semantics); the tag is re-encoded for the key.
func (h *Handler) HandleRepo(w http.ResponseWriter, r *http.Request, id git.RepoId, sub []string) bool {
	if len(sub) != 4 || sub[0] != "releases" || sub[2] != "assets" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return true
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	h.serveAsset(w, r, id.Owner, id.Name, p, sub[1], sub[3])
	return true
}

// splitPath splits the escaped path and decodes each segment separately.
func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		out = append(out, decodeSegment(s))
	}
	return out
}

// decodeSegment decodes one path segment; an undecodable segment survives
// verbatim (fail closed downstream: it won't match a tag or name shape).
func decodeSegment(s string) string {
	d, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return d
}

// --- writers ---------------------------------------------------------------

func writePlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Del("ETag")
	if status == http.StatusUnauthorized {
		hdr.Set("WWW-Authenticate", `Bearer realm="walgit"`)
	}
	if status == http.StatusServiceUnavailable {
		hdr.Set("Retry-After", "15")
	}
	hdr.Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func writeErr(w http.ResponseWriter, err error) {
	if aerr, ok := err.(*auth.AuthError); ok {
		switch aerr.Kind {
		case auth.ErrForbidden:
			writePlain(w, http.StatusForbidden, aerr.Why)
		case auth.ErrUnavailable:
			writePlain(w, http.StatusServiceUnavailable, aerr.Why)
		default:
			writePlain(w, http.StatusUnauthorized, aerr.Why)
		}
		return
	}
	writePlain(w, statusFor(err), err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "releases: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", ccNoStore)
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeCached writes JSON with a cache class + ETag/304 path.
func writeCached(w http.ResponseWriter, r *http.Request, class, etag string, status int, v any) {
	hdr := w.Header()
	hdr.Set("Cache-Control", class)
	if etag != "" {
		hdr.Set("ETag", `"`+etag+`"`)
		if matchETag(r.Header.Get("If-None-Match"), etag) {
			hdr.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "releases: encode: "+err.Error())
		return
	}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func matchETag(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, "W/")
		p = strings.Trim(p, `"`)
		if p == etag {
			return true
		}
	}
	return false
}

const (
	ccSWR     = "private, max-age=0, stale-while-revalidate=60"
	ccNoStore = "no-store"
)

// decodeStrict unmarshals body into v after rejecting unknown top-level
// keys (fail closed: unknown keys on write are 400, on read ignored).
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, allowed map[string]bool, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	if keys == nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: expected an object")
		return false
	}
	for k := range keys {
		if !allowed[k] {
			writePlain(w, http.StatusBadRequest, "unknown field "+strconv.Quote(k))
			return false
		}
	}
	if err := json.Unmarshal(body, v); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
}

// --- routing ---------------------------------------------------------------

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	// rest[0] == "releases" (claimed by Handle). "latest" and
	// "autodraft" are reserved single-segment names (matched before the
	// generic tag routes): a tag literally named "latest" is creatable
	// and deletable but its GET single is shadowed by the pointer (it
	// stays visible in the list) — same class of quirk as any fixed
	// sub-route; tags named "autodraft" behave identically.
	switch {
	case len(rest) == 1 && r.Method == http.MethodGet:
		h.listReleases(w, r, owner, repo, p)
	case len(rest) == 2 && rest[1] == "latest" && r.Method == http.MethodGet:
		h.latestRelease(w, r, owner, repo, p)
	case len(rest) == 2 && rest[1] == "autodraft" && r.Method == http.MethodGet:
		h.autodraft(w, r, owner, repo, p)
	case len(rest) == 2 && r.Method == http.MethodGet:
		h.getRelease(w, r, owner, repo, p, rest[1])
	case len(rest) == 2 && r.Method == http.MethodPut:
		h.putRelease(w, r, owner, repo, p, rest[1])
	case len(rest) == 2 && r.Method == http.MethodDelete:
		h.deleteRelease(w, r, owner, repo, p, rest[1])
	case len(rest) == 4 && rest[2] == "assets" && r.Method == http.MethodPost:
		h.uploadAsset(w, r, owner, repo, p, rest[1], rest[3])
	case len(rest) == 4 && rest[2] == "assets" && r.Method == http.MethodDelete:
		h.deleteAsset(w, r, owner, repo, p, rest[1], rest[3])
	case len(rest) == 2:
		methodNotAllowed(w, "GET", "PUT", "DELETE")
	case len(rest) == 4 && rest[2] == "assets":
		methodNotAllowed(w, "POST", "DELETE")
	default:
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// --- wire ------------------------------------------------------------------

// assetWire is one asset entry plus its download URL (§7: Release JSON on
// the wire = the §1.1 body + browser_download_url per asset).
type assetWire struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	SHA256             string `json:"sha256"`
	ContentType        string `json:"content_type"`
	UploadedAt         string `json:"uploaded_at"`
	Uploader           string `json:"uploader"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// releaseWire is the §1.1 body on the wire (assets [] never null).
type releaseWire struct {
	Tag         string      `json:"tag"`
	TagSHA      string      `json:"tag_sha"`
	Name        string      `json:"name"`
	Body        string      `json:"body"`
	Draft       bool        `json:"draft"`
	Prerelease  bool        `json:"prerelease"`
	Author      string      `json:"author"`
	CreatedAt   string      `json:"created_at"`
	PublishedAt *string     `json:"published_at"`
	UpdatedAt   string      `json:"updated_at"`
	Assets      []assetWire `json:"assets"`
}

func downloadURL(owner, repo, tag, name string) string {
	return "/" + owner + "/" + repo + "/releases/" +
		url.PathEscape(tag) + "/assets/" + url.PathEscape(name)
}

func wireRelease(owner, repo string, r *Release) releaseWire {
	assets := make([]assetWire, 0, len(r.Assets))
	for _, a := range r.Assets {
		assets = append(assets, assetWire{
			Name: a.Name, Size: a.Size, SHA256: a.SHA256,
			ContentType: a.ContentType, UploadedAt: a.UploadedAt,
			Uploader:           a.Uploader,
			BrowserDownloadURL: downloadURL(owner, repo, r.Tag, a.Name),
		})
	}
	return releaseWire{
		Tag: r.Tag, TagSHA: r.TagSHA, Name: r.Name, Body: r.Body,
		Draft: r.Draft, Prerelease: r.Prerelease, Author: r.Author,
		CreatedAt: r.CreatedAt, PublishedAt: r.PublishedAt,
		UpdatedAt: r.UpdatedAt, Assets: assets,
	}
}

// --- endpoints --------------------------------------------------------------

func (h *Handler) listReleases(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	q := r.URL.Query()
	n := ListDefaultPage
	if s := q.Get("n"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			writePlain(w, http.StatusBadRequest, "invalid n")
			return
		}
		n = v
	}
	rels, more, err := h.Svc.ListReleases(r.Context(), owner, repo, p, n, q.Get("after"))
	if err != nil {
		writeErr(w, err)
		return
	}
	wire := make([]releaseWire, 0, len(rels))
	var etagParts []string
	for _, h := range rels {
		wire = append(wire, wireRelease(owner, repo, h.rel))
		etagParts = append(etagParts, h.rel.Tag+"/"+string(h.ver))
	}
	writeCached(w, r, ccSWR, listETag(etagParts), http.StatusOK,
		map[string]any{"releases": wire, "more": more})
}

// listETag folds the page's (tag, version) pairs into one ETag token.
func listETag(parts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:32]
}

func (h *Handler) latestRelease(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	rel, ver, err := h.Svc.LatestRelease(r.Context(), owner, repo, p, r.URL.Query().Get("include_prereleases") == "1")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeCached(w, r, ccSWR, string(ver), http.StatusOK, wireRelease(owner, repo, rel))
}

func (h *Handler) getRelease(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal, tag string) {
	rel, ver, err := h.Svc.GetRelease(r.Context(), owner, repo, p, tag)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeCached(w, r, ccSWR, string(ver), http.StatusOK, wireRelease(owner, repo, rel))
}

var putReleaseFields = map[string]bool{"name": true, "body": true, "draft": true, "prerelease": true}

func (h *Handler) putRelease(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal, tag string) {
	var in ReleaseInput
	if !decodeStrict(w, r, MaxBodyBytes+4096, putReleaseFields, &in) {
		return
	}
	rel, created, err := h.Svc.PutRelease(r.Context(), owner, repo, p, tag, in, store.Version(matchToken(r.Header.Get("If-Match"))))
	if err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, wireRelease(owner, repo, rel))
}

// matchToken unwraps one If-Match token ("" = no constraint; "*" passes
// through for the service's create-only interpretation).
func matchToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" || header == "*" {
		return header
	}
	if i := strings.Index(header, ","); i >= 0 {
		header = header[:i]
	}
	header = strings.TrimSpace(header)
	header = strings.TrimPrefix(header, "W/")
	return strings.Trim(header, `"`)
}

func (h *Handler) deleteRelease(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal, tag string) {
	if err := h.Svc.DeleteRelease(r.Context(), owner, repo, p, tag); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tag": tag})
}

func (h *Handler) autodraft(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	q := r.URL.Query()
	tag := q.Get("tag")
	if tag == "" {
		writePlain(w, http.StatusBadRequest, "tag is required")
		return
	}
	ad, err := h.Svc.Autodraft(r.Context(), owner, repo, p, tag, q.Get("since"))
	if err != nil {
		writeErr(w, err)
		return
	}
	prs := ad.PRs
	writeCached(w, r, ccSWR, "", http.StatusOK, map[string]any{
		"tag": ad.Tag, "since": ad.Since, "body": ad.Body, "prs": prs, "more": ad.More,
	})
}

func (h *Handler) uploadAsset(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal, tag, name string) {
	sha := r.Header.Get("X-Walgit-Asset-Sha256")
	if sha == "" {
		writePlain(w, http.StatusBadRequest, "X-Walgit-Asset-Sha256 is required")
		return
	}
	entry, err := h.Svc.UploadAsset(r.Context(), owner, repo, p, tag, name, r.Body,
		r.ContentLength, sha, r.Header.Get("Content-Type"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, assetWire{
		Name: entry.Name, Size: entry.Size, SHA256: entry.SHA256,
		ContentType: entry.ContentType, UploadedAt: entry.UploadedAt,
		Uploader:           entry.Uploader,
		BrowserDownloadURL: downloadURL(owner, repo, tag, entry.Name),
	})
}

func (h *Handler) deleteAsset(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal, tag, name string) {
	entry, err := h.Svc.DeleteAsset(r.Context(), owner, repo, p, tag, name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, assetWire{
		Name: entry.Name, Size: entry.Size, SHA256: entry.SHA256,
		ContentType: entry.ContentType, UploadedAt: entry.UploadedAt,
		Uploader:           entry.Uploader,
		BrowserDownloadURL: downloadURL(owner, repo, tag, entry.Name),
	})
}

// --- asset bytes: the static contract (§1.2, 06_server_http §5) ------------

// serveAsset serves GET|HEAD …/releases/{tag}/assets/{name}: strong ETag
// from the store version, If-None-Match → 304, Range + If-Range → 206/416,
// HEAD, immutable caching, nosniff. Content-Type is the declared type from
// the release header (the byte object itself carries no metadata). Served
// direct (no compress group wraps repoDispatch; accel offload stays an
// edge concern — the object is immutable-addressed either way).
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal, tag, name string) {
	if err := h.Svc.requireRead(r.Context(), owner, repo, p); err != nil {
		writeErr(w, err)
		return
	}
	tag, err := validateTag(tag)
	if err != nil {
		writeErr(w, err)
		return
	}
	name, err = validateAssetName(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	hraw, _, err := h.Svc.getJSON(r.Context(), ReleaseKey(owner, repo, tag))
	if err != nil {
		writeErr(w, err)
		return
	}
	if hraw == nil {
		writePlain(w, http.StatusNotFound, "not found")
		return
	}
	rel, perr := parseRelease(hraw)
	if perr != nil {
		writeErr(w, perr)
		return
	}
	i := findAsset(rel, name)
	if i < 0 {
		writePlain(w, http.StatusNotFound, "not found")
		return
	}
	key := AssetKey(owner, repo, tag, name)
	meta, merr := h.Svc.Store.Head(r.Context(), key)
	if merr != nil {
		if store.IsNotFound(merr) {
			writePlain(w, http.StatusNotFound, "not found")
			return
		}
		writePlain(w, http.StatusServiceUnavailable, merr.Error())
		return
	}
	if meta == nil {
		writePlain(w, http.StatusNotFound, "not found")
		return
	}
	version := string(meta.Version)
	size := meta.Size

	hdr := w.Header()
	hdr.Set("ETag", `"`+version+`"`)
	hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
	hdr.Set("Accept-Ranges", "bytes")
	hdr.Set("Content-Type", rel.Assets[i].ContentType)
	hdr.Set("X-Content-Type-Options", "nosniff")

	if inm := r.Header.Get("If-None-Match"); inm != "" && matchETag(inm, version) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rng := r.Header.Get("Range")
	// Range handling is defined for GET only (RFC 7233 §3.1: ignore it
	// on any other method — HEAD answers full-object headers).
	if r.Method != http.MethodGet {
		rng = ""
	} else if rng != "" {
		if ir := r.Header.Get("If-Range"); ir != "" && !matchETag(ir, version) {
			rng = "" // validator differs → full 200
		}
	}
	if rng == "" {
		hdr.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		h.streamBytes(w, r, key, store.GetOptions{})
		return
	}
	start, end, ok := parseRange(rng, size)
	if !ok {
		hdr.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		writePlain(w, http.StatusRequestedRangeNotSatisfiable, "range not satisfiable")
		return
	}
	hdr.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+
		"/"+strconv.FormatInt(size, 10))
	hdr.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	h.streamBytes(w, r, key, store.GetOptions{Range: &[2]int64{start, end + 1}})
}

// streamBytes copies the store object body; errors mid-stream are terminal
// (the status is already sent).
func (h *Handler) streamBytes(w http.ResponseWriter, r *http.Request, key string, opts store.GetOptions) {
	res, err := h.Svc.Store.Get(r.Context(), key, opts)
	if err != nil {
		return
	}
	obj, ok := res.(store.Object)
	if !ok {
		return
	}
	defer obj.Body.Close()
	_, _ = io.Copy(w, obj.Body)
}

// parseRange parses a single RFC 7233 bytes range into a closed
// [start,end] against size (same semantics as the server's static path:
// unsatisfiable or multiple ranges ⇒ ok=false → 416).
func parseRange(spec string, size int64) (start, end int64, ok bool) {
	if len(spec) < 6 || !strings.EqualFold(spec[:6], "bytes=") {
		return 0, 0, false
	}
	spec = spec[6:]
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	first, last := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	if first == "" {
		if last == "" {
			return 0, 0, false
		}
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 || size == 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	s0, err := strconv.ParseInt(first, 10, 64)
	if err != nil || s0 < 0 || s0 >= size {
		return 0, 0, false
	}
	if last == "" {
		return s0, size - 1, true
	}
	e0, err := strconv.ParseInt(last, 10, 64)
	if err != nil || e0 < s0 {
		return 0, 0, false
	}
	if e0 >= size {
		e0 = size - 1
	}
	return s0, e0, true
}
