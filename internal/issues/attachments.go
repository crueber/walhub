package issues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// §12 attachments: pasted/dropped issue images (Forgejo #120).
//
// Bodies stay raw text ≤ 64 KiB (validateBody) — images are stored objects
// referenced by markdown links, never inline data: URIs (the sanitizer
// drops data: URLs, so inline paste could never render).
//
// Bucket family (new, content-addressed, Create-only immutable):
//
//	repos/<o>/<r>/attachments/<sha256hex>/<name>   raw image bytes
//
// Concurrent pastes of the same bytes converge via create-if-absent: a 412
// under the same key is same-bytes by construction (the key embeds the
// hash), so it is idempotent success — no CAS loop, no header object, no
// locks. Orphans (upload without a later comment-submit) are kept, same
// philosophy as release assets: bytes first, reference second. A future
// `attachment-sweep` maintainer kind may reconcile bodies against
// attachments when the family grows.
//
// ### Concurrency
//
// Hazard: concurrent identical uploads racing on one content-addressed key.
// Avoidance: the store's create-if-absent arbitrates; losers take the 412
// as idempotent 201 (same key ⇒ same bytes — no re-read, no compare).
// Distinct bytes never collide (hash in key). The handler holds no repo
// locks across store calls (13 §2 rule 4); spool/verify/Create is lock-free
// straight-line I/O on the request goroutine, bounded by the 8 MiB cap.
// Attachment byte PUTs are small (≤ 8 MiB, single object, no striping), so
// control-plane transport is fine — no bulk-pool involvement (13 §7).

// DefaultMaxImageBytes caps one attachment upload when unconfigured
// (attachments.max_image_bytes, default 8 MiB → 413 over cap).
const DefaultMaxImageBytes = int64(8 << 20)

// MaxAttachmentNameLen bounds the sanitized filename segment (1–200 bytes,
// same bound as release asset names).
const MaxAttachmentNameLen = 200

// AttachmentRecord is the POST …/api/attachments 201 body (07 §2: JSON
// success; url is the GET byte path below).
type AttachmentRecord struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
}

// AttachmentKey returns repos/<o>/<r>/attachments/<sha>/<name>.
func AttachmentKey(owner, repo, sha, name string) string {
	return "repos/" + owner + "/" + repo + "/attachments/" + sha + "/" + name
}

// AttachmentsPrefix returns repos/<o>/<r>/attachments/ (sweep root, §12).
func AttachmentsPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/attachments/"
}

// maxImageBytes resolves the upload cap (0 = compiled-in default).
func (s *Service) maxImageBytes() int64 {
	if s.MaxImageBytes > 0 {
		return s.MaxImageBytes
	}
	return DefaultMaxImageBytes
}

// sanitizeAttachmentName validates one filename segment: 1–200 bytes, no
// path separators, no leading dot, never . or ... Callers pass the already
// segment-decoded name (Handle decodes per segment; the ?name= query value
// arrives decoded from r.URL.Query()).
func sanitizeAttachmentName(name string) (string, error) {
	t := strings.TrimSpace(name)
	if t == "" || t == "." || t == ".." {
		return "", fmt.Errorf("%w: attachment name must not be empty", ErrInvalid)
	}
	if len(t) > MaxAttachmentNameLen {
		return "", fmt.Errorf("%w: attachment name exceeds %d bytes", ErrInvalid, MaxAttachmentNameLen)
	}
	if strings.HasPrefix(t, ".") {
		return "", fmt.Errorf("%w: attachment name must not start with '.'", ErrInvalid)
	}
	if strings.ContainsAny(t, "/\\") || strings.ContainsRune(t, 0) {
		return "", fmt.Errorf("%w: attachment name must be a single path segment", ErrInvalid)
	}
	for _, r := range t {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: attachment name must not contain control characters", ErrInvalid)
		}
	}
	return t, nil
}

// sniffImageType sniffs the PNG/JPEG/GIF/WebP allowlist from magic bytes
// (never the extension, never the client Content-Type). SVG is rejected:
// same-origin served SVG is script execution in our origin, and the
// dependency budget forbids pulling in an XML sanitizer. Anything outside
// the allowlist (including SVG/XML text) reports ok=false → 415.
func sniffImageType(head []byte) (ct string, ok bool) {
	if len(head) >= 8 &&
		head[0] == 0x89 && head[1] == 0x50 && head[2] == 0x4E && head[3] == 0x47 &&
		head[4] == 0x0D && head[5] == 0x0A && head[6] == 0x1A && head[7] == 0x0A {
		return "image/png", true
	}
	if len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF {
		return "image/jpeg", true
	}
	if len(head) >= 6 && head[0] == 'G' && head[1] == 'I' && head[2] == 'F' &&
		head[3] == '8' && (head[4] == '7' || head[4] == '9') && head[5] == 'a' {
		return "image/gif", true
	}
	if len(head) >= 12 && head[0] == 'R' && head[1] == 'I' && head[2] == 'F' && head[3] == 'F' &&
		head[8] == 'W' && head[9] == 'E' && head[10] == 'B' && head[11] == 'P' {
		return "image/webp", true
	}
	return "", false
}

// extForType is the fallback extension when the client sends no name.
func extForType(ct string) string {
	switch ct {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".bin"
}

// normalizeSHA256 lowercases and validates a 64-hex sha256; "" stays ""
// (the header is optional-when-present per S4: non-secure origins where
// crypto.subtle is undefined upload without it, and the server always
// hashes the spool itself).
func normalizeSHA256(hexStr string) (string, error) {
	if hexStr == "" {
		return "", nil
	}
	s := strings.ToLower(strings.TrimSpace(hexStr))
	if len(s) != 64 {
		return "", fmt.Errorf("%w: X-Walgit-Attachment-Sha256 must be 64 hex characters", ErrInvalid)
	}
	for i := range s {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("%w: X-Walgit-Attachment-Sha256 must be hex", ErrInvalid)
		}
	}
	return s, nil
}

// UploadAttachment stores one image upload (single-step: images are ≤ 8
// MiB, bounded, human-rate — not the release two-step). Auth: read
// (authenticated) — the same gate as AddComment, so anonymous uploads are
// 401 and the upload surface never exceeds the comment surface (S7).
//
// Round trips: 1 Create on the content-addressed key (a 412 dedup-hit
// returns the same record with no re-read). Upload and comment-submit are
// decoupled: the upload references no issue number, so the new-issue form
// (whose issue does not exist yet) and thread comments share the path.
func (s *Service) UploadAttachment(ctx context.Context, owner, repo string, p auth.Principal, name string, body io.Reader, declaredSHA string) (*AttachmentRecord, error) {
	if err := requireAuthenticated(p); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	want, err := normalizeSHA256(declaredSHA)
	if err != nil {
		return nil, err
	}
	max := s.maxImageBytes()

	// Spool-verify: stream to a spool file while hashing (LFS §6.2
	// pattern: never buffer the upload in memory). The cap is enforced
	// during the stream (no Content-Length requirement — chunked clients
	// work; B1), so a lying length cannot over-allocate.
	spoolDir := s.SpoolDir
	if spoolDir == "" {
		spoolDir = os.TempDir()
	}
	if merr := os.MkdirAll(spoolDir, 0o700); merr != nil {
		return nil, merr
	}
	tmp, terr := os.CreateTemp(spoolDir, "attachment-*")
	if terr != nil {
		return nil, terr
	}
	spoolName := tmp.Name()
	defer os.Remove(spoolName) //nolint:errcheck — best-effort cleanup
	defer tmp.Close()          //nolint:errcheck — closed before the backend reads
	h := sha256.New()
	n, cerr := io.Copy(tmp, io.TeeReader(io.LimitReader(body, max+1), h))
	if cerr != nil {
		return nil, fmt.Errorf("%w: unreadable upload", ErrInvalid)
	}
	if n > max {
		return nil, fmt.Errorf("%w: upload exceeds %d bytes", ErrTooLarge, max)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if want != "" && want != sha {
		return nil, fmt.Errorf("%w: sha256 mismatch", ErrInvalid)
	}
	if serr := tmp.Sync(); serr != nil {
		return nil, serr
	}
	if cerr := tmp.Close(); cerr != nil {
		return nil, cerr
	}

	// Sniff from the spool head (magic bytes, never the extension).
	head := make([]byte, 512)
	f, ferr := os.Open(spoolName)
	if ferr != nil {
		return nil, ferr
	}
	hn, _ := io.ReadFull(f, head)
	_ = f.Close()
	ct, ok := sniffImageType(head[:hn])
	if !ok {
		return nil, fmt.Errorf("%w: only PNG, JPEG, GIF, and WebP images are accepted", ErrUnsupportedMedia)
	}

	effName := strings.TrimSpace(name)
	if effName == "" {
		effName = "image" + extForType(ct)
	}
	effName, err = sanitizeAttachmentName(effName)
	if err != nil {
		return nil, err
	}

	key := AttachmentKey(owner, repo, sha, effName)
	rec := &AttachmentRecord{
		Name: effName, Size: n, SHA256: sha, ContentType: ct,
		URL: "/" + owner + "/" + repo + "/attachments/" + sha + "/" + url.PathEscape(effName),
	}
	// Bytes Create arbitrates races: a 412 under this key is same-bytes
	// by construction (hash in key) → idempotent 201, no re-read.
	if _, perr := s.Store.Put(ctx, key, store.PutBody{File: spoolName},
		store.PutOptions{Mode: store.PutCreate, ContentType: ct, Immutable: true}); perr != nil {
		if !store.IsPreconditionFailed(perr) {
			return nil, perr
		}
	}
	return rec, nil
}

// attachmentShape validates the GET path segments (fail closed: bad sha
// or bad name → 404 unknown attachment, same contract as parseNum →
// unknown issue).
func attachmentShape(shaSeg, nameSeg string) (sha, name string, ok bool) {
	sha = strings.ToLower(strings.TrimSpace(shaSeg))
	if len(sha) != 64 {
		return "", "", false
	}
	for i := range sha {
		c := sha[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", false
		}
	}
	name, err := sanitizeAttachmentName(nameSeg)
	if err != nil {
		return "", "", false
	}
	return sha, name, true
}

// serveAttachment serves GET|HEAD /{o}/{r}/attachments/<sha>/<name> under
// the in-package static contract (B3: importing server would be an upward
// import into the package that wires this one — law 8): strong ETag from
// the store version, If-None-Match → 304, Range + If-Range → 206/416, HEAD,
// Accept-Ranges, sniffed Content-Type (re-sniffed from the stored prefix:
// the store contract exposes no content type on read, and the bytes are
// immutable so the sniff is deterministic), X-Content-Type-Options:
// nosniff. One deliberate deviation from 06 §5: Cache-Control is private,
// not public — immutable bytes, but authenticated reads must not sit in
// shared caches. Reads gate at read level (private-repo screenshots must
// not leak via URL guessing; the URL is unguessable but that is not a
// security boundary).
//
// Round trips: warm GET/HEAD = 1 Get (conditional + range decided from the
// returned meta); a satisfiable byte range costs one follow-up ranged Get.
// HandleRepo serves the repo-subpath byte route (§12): GET|HEAD
// /{o}/{r}/attachments/<sha>/<name>, registered on the server's ChainRepo
// chain (repo_extra.go — the seam for byte families outside the api lanes,
// same as release asset bytes). No git sub-path is literally
// "attachments", and the segment sits after {o}/{r} so no repo-name clash;
// anything else reports false so the core mux answers. Authentication
// resolves in-handler (byte routes are outside the api seam, so the lane
// principal injection does not apply — same as releases).
func (h *Handler) HandleRepo(w http.ResponseWriter, r *http.Request, id git.RepoId, sub []string) bool {
	if len(sub) != 3 || sub[0] != "attachments" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET", "HEAD")
		return true
	}
	h.serveAttachment(w, r, id.Owner, id.Name, sub[1], sub[2])
	return true
}

func (h *Handler) serveAttachment(w http.ResponseWriter, r *http.Request, owner, repo, shaSeg, nameSeg string) {
	sha, name, ok := attachmentShape(shaSeg, nameSeg)
	if !ok {
		writePlain(w, http.StatusNotFound, "unknown attachment")
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.requireRead(r.Context(), owner, repo, p); err != nil {
		writeErr(w, err)
		return
	}
	key := AttachmentKey(owner, repo, sha, name)
	// Conditional read: concrete ETag tokens ride the store
	// IfNoneMatch (exact version equality); "*" matches any current
	// version and is decided below once the object is in hand.
	inmHeader := r.Header.Get("If-None-Match")
	var inmOpt store.Version
	if tok := etagVersion(inmHeader); tok != "" && tok != "*" {
		inmOpt = store.Version(tok)
	}
	res, gerr := h.Svc.Store.Get(r.Context(), key, store.GetOptions{IfNoneMatch: inmOpt})
	if gerr != nil {
		if store.IsNotFound(gerr) {
			writePlain(w, http.StatusNotFound, "unknown attachment")
			return
		}
		writePlain(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if nm, isNM := res.(store.NotModified); isNM {
		writeAttachmentHeaders(w, string(nm.Version), "", 0)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	obj, isObj := res.(store.Object)
	if !isObj {
		writePlain(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	defer obj.Body.Close()
	version := string(obj.Meta.Version)
	size := obj.Meta.Size
	if strings.TrimSpace(inmHeader) == "*" || matchETag(inmHeader, version) {
		writeAttachmentHeaders(w, version, "", 0)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Buffer the sniff prefix (≤ 512 B from the file start — this Get
	// carries no Range, so the stream always starts at byte 0).
	prefix := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for len(prefix) < 512 {
		n, rerr := obj.Body.Read(tmp[len(prefix):])
		if n > 0 {
			prefix = append(prefix, tmp[len(prefix):len(prefix)+n]...)
		}
		if rerr != nil {
			break
		}
	}
	ct, ok := sniffImageType(prefix)
	if !ok {
		writePlain(w, http.StatusServiceUnavailable, "corrupt object")
		return
	}

	rng := r.Header.Get("Range")
	if rng != "" && r.Method != http.MethodHead {
		if ir := r.Header.Get("If-Range"); ir != "" && !matchETag(ir, version) {
			rng = "" // validator differs → full 200
		}
	}
	if rng == "" {
		writeAttachmentHeaders(w, version, ct, size)
		hdr := w.Header()
		hdr.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		if len(prefix) > 0 {
			_, _ = w.Write(prefix)
		}
		_, _ = io.Copy(w, obj.Body)
		return
	}
	start, end, rok := parseAttachmentRange(rng, size)
	obj.Body.Close()
	if !rok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		writePlain(w, http.StatusRequestedRangeNotSatisfiable, "range not satisfiable")
		return
	}
	rres, rerr := h.Svc.Store.Get(r.Context(), key, store.GetOptions{Range: &[2]int64{start, end + 1}})
	if rerr != nil {
		writePlain(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	robj, isRObj := rres.(store.Object)
	if !isRObj {
		writePlain(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	defer robj.Body.Close()
	writeAttachmentHeaders(w, version, ct, size)
	hdr := w.Header()
	hdr.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+
		"/"+strconv.FormatInt(size, 10))
	hdr.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.Copy(w, robj.Body)
}

// writeAttachmentHeaders sets the byte-GET cache class (B3: private,
// immutable — authenticated reads never sit in shared caches).
func writeAttachmentHeaders(w http.ResponseWriter, version, contentType string, _ int64) {
	hdr := w.Header()
	if version != "" {
		hdr.Set("ETag", `"`+version+`"`)
	}
	hdr.Set("Cache-Control", "private, max-age=31536000, immutable")
	hdr.Set("Accept-Ranges", "bytes")
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	hdr.Set("X-Content-Type-Options", "nosniff")
}

// etagVersion extracts a bare version token for the store IfNoneMatch
// (strong comparison; "*" matches any current version).
func etagVersion(header string) string {
	header = strings.TrimSpace(header)
	if header == "*" {
		return "*"
	}
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, "W/")
		p = strings.Trim(p, `"`)
		if p != "" {
			return p
		}
	}
	return ""
}

// parseAttachmentRange parses a single RFC 7233 bytes range into a closed
// [start,end] against size (same semantics as the server static path:
// suffix form supported; multiple ranges → full 200 via ok=false… here
// ok=false → 416; callers serving full on multi-range do so explicitly).
func parseAttachmentRange(spec string, size int64) (start, end int64, ok bool) {
	if len(spec) < 6 || (spec[0] != 'b' && spec[0] != 'B') {
		return 0, 0, false
	}
	low := strings.ToLower(spec)
	if !strings.HasPrefix(low, "bytes=") {
		return 0, 0, false
	}
	rest := spec[len("bytes="):]
	if strings.Contains(rest, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(rest, '-')
	if dash < 0 {
		return 0, 0, false
	}
	first, last := strings.TrimSpace(rest[:dash]), strings.TrimSpace(rest[dash+1:])
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

// uploadAttachment serves POST /{o}/{r}/api/attachments (both lanes):
// raw image bytes with an optional ?name= and an optional
// X-Walgit-Attachment-Sha256 (verified only when present — S4). 201 with
// the JSON record; plain-text errors (07 §2); POST is no-store.
func (h *Handler) uploadAttachment(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	rec, err := h.Svc.UploadAttachment(r.Context(), owner, repo, p, r.URL.Query().Get("name"), r.Body, r.Header.Get("X-Walgit-Attachment-Sha256"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

// attachmentSpoolDir resolves the spool parent for composition (mirrors
// the releases release-spool wiring).
func attachmentSpoolDir(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "attachments-spool")
}
